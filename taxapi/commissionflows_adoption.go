package taxapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"
)

// CommissionCaptureReference adopts evidence by reference, never by updating an
// existing generation. It is collection provenance, not financial acceptance.
// Concurrent old collectors can still finish the same root. Their results must
// remain retained and the union must count the canonical root only once.
type CommissionCaptureReference struct {
	Version       string
	Build         string
	ProfileSHA256 string
	Chain         string
	RootSHA256    string
	PayloadSHA256 map[string]string
}

// ResolveCommissionCapture checks only explicit source versions, using the
// target's exact build, combined profile, chain and dates. No migration, native
// source query, INSERT, UPDATE or DELETE is performed. Every chunk is verified
// by ReadCommissionFlowPayload before a reference is returned.
func (s *Server) ResolveCommissionCapture(target CommissionFlowBuildOptions, sources []string, chain, validator string, from time.Time) (*CommissionCaptureReference, error) {
	if err := target.validate(); err != nil {
		return nil, err
	}
	if len(sources) > 8 {
		return nil, fmt.Errorf("capture source allowlist exceeds candidate bound")
	}
	ctx, cancel := context.WithTimeout(s.db.Statement.Context, 60*time.Second)
	defer cancel()
	// A database-only facade: the reads below (and ReadCommissionFlowPayload)
	// use only db, and copying the Server would copy its locks.
	s = &Server{db: s.db.WithContext(ctx)}
	seen := map[string]bool{target.Version: true}
	for _, version := range sources {
		if version == "" || seen[version] {
			return nil, fmt.Errorf("capture source versions must be explicit and distinct from target")
		}
		seen[version] = true
	}
	for _, version := range sources {
		var snapshot CommissionFlowVersion
		err := s.db.Where("version = ? AND chain = ? AND validator = ?", version, chain, validator).Take(&snapshot).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("capture source lookup failed")
		}
		expected := target
		expected.Version = version
		if err := expected.validate(); err != nil {
			return nil, err
		}
		var run bytes.Buffer
		if err := s.ReadCommissionFlowPayload(snapshot, "run", &run); err != nil {
			return nil, fmt.Errorf("capture source run verification failed")
		}
		checked := snapshot
		checked.RunJSON = run.String()
		var parsed CommissionFlowRun
		if err := json.Unmarshal(run.Bytes(), &parsed); err != nil {
			return nil, fmt.Errorf("invalid immutable commission source run")
		}
		if parsed.Status == flowStatusError {
			continue
		}
		if err := reusableCommissionFlowVersion(checked, expected, chain, validator, from); err != nil {
			return nil, err
		}
		ref := &CommissionCaptureReference{Version: version, Build: snapshot.Build, ProfileSHA256: snapshot.PrivateProfileSHA256, Chain: chain, RootSHA256: privateSHA([]byte(validator)), PayloadSHA256: map[string]string{}}
		for _, field := range []string{"run", "rows", "evidence"} {
			hash := sha256.New()
			if field == "run" {
				_, _ = io.Copy(hash, bytes.NewReader(run.Bytes()))
			} else if err := s.ReadCommissionFlowPayload(snapshot, field, hash); err != nil {
				return nil, fmt.Errorf("capture source payload verification failed")
			}
			ref.PayloadSHA256[field] = hex.EncodeToString(hash.Sum(nil))
		}
		return ref, nil
	}
	return nil, nil
}

// CommissionCaptureUnion counts roots, not attempts or financial rows. Sources
// retain caller priority and duplicates remain visible in the returned count.
// References from separate roots, builds or profiles cannot be combined.
func CommissionCaptureUnion(refs []CommissionCaptureReference) (CommissionCaptureReference, int, error) {
	if len(refs) == 0 {
		return CommissionCaptureReference{}, 0, fmt.Errorf("no capture references")
	}
	first := refs[0]
	for _, ref := range refs {
		if ref.Version == "" || ref.Build == "" || ref.Chain == "" || !correctionHash(ref.RootSHA256) || !correctionHash(ref.ProfileSHA256) || ref.RootSHA256 != first.RootSHA256 || ref.Build != first.Build || ref.ProfileSHA256 != first.ProfileSHA256 || ref.Chain != first.Chain || len(ref.PayloadSHA256) != 3 {
			return CommissionCaptureReference{}, 0, fmt.Errorf("incompatible capture references")
		}
		for _, field := range []string{"run", "rows", "evidence"} {
			if !correctionHash(ref.PayloadSHA256[field]) {
				return CommissionCaptureReference{}, 0, fmt.Errorf("unverified capture reference")
			}
		}
	}
	// Payload differences are preserved as distinct evidence, never represented as
	// financial equivalence. The first verified source is the collection reference.
	return first, len(refs) - 1, nil
}

// MarshalCommissionCaptureReference contains no raw validator or financial rows.
func MarshalCommissionCaptureReference(ref CommissionCaptureReference) ([]byte, error) {
	if _, _, err := CommissionCaptureUnion([]CommissionCaptureReference{ref}); err != nil {
		return nil, err
	}
	return json.Marshal(ref)
}

// CommissionAccelerationHeadroom is a fresh observer sample, not a node CPU
// percentage. Policy limits belong to the candidate and do not change PG quota.
type CommissionAccelerationHeadroom struct {
	ObservedAt                time.Time
	SampleDuration            time.Duration
	PGQuotaCores              float64
	PGUsedCores               float64
	PGResidentBytes           int64
	PGMemoryCurrentBytes      int64
	PGInactiveFileBytes       int64
	PGAnonBytes               int64
	PGShmemBytes              int64
	PGKernelBytes             int64
	PGOOMKills                int64
	PGMemoryPressureFullAvg10 float64
	PGMemoryLimitBytes        int64
	JobUsedBytes              int64
	JobMemoryLimitBytes       int64
	NodeAvailableBytes        int64
	AdmissionGranted          bool
	PublicHealthy             bool
	ReplayHealthy             bool
}

// Allow refuses stale, invalid or exhausted headroom. A caller must continuously
// resample and cancel only its own new worker on refusal, not pause old actors.
func (h CommissionAccelerationHeadroom) Allow(now time.Time, reserveCores float64, reserveBytes int64) bool {
	age := now.Sub(h.ObservedAt)
	return age >= 0 && age <= 5*time.Second && h.SampleDuration >= time.Second && h.SampleDuration <= 30*time.Second && reserveCores > 0 && reserveBytes > 0 &&
		h.PGQuotaCores > 0 && h.PGUsedCores >= 0 && h.PGUsedCores+reserveCores <= h.PGQuotaCores &&
		h.PGMemoryCurrentBytes >= h.PGInactiveFileBytes && h.PGInactiveFileBytes >= 0 && h.PGResidentBytes == h.PGMemoryCurrentBytes-h.PGInactiveFileBytes &&
		h.PGResidentBytes >= 0 && h.PGResidentBytes <= h.PGMemoryLimitBytes-reserveBytes && h.PGOOMKills == 0 && h.PGMemoryPressureFullAvg10 >= 0 && h.PGMemoryPressureFullAvg10 <= 10 &&
		h.JobUsedBytes >= 0 && h.JobUsedBytes <= h.JobMemoryLimitBytes-reserveBytes && h.NodeAvailableBytes >= 8<<30 && h.AdmissionGranted && h.PublicHealthy && h.ReplayHealthy
}
