// taxcommissionaccelerate schedules fresh private roots around an unchanged
// native executable. It never changes old collectors or financial acceptance.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
)

type config struct {
	Version, Build, Chain, From, AsOf                                          string
	SourceVersions                                                             []string
	ComputeOnly                                                                bool
	CanonicalFile, CanonicalSHA256, QueueFile, QueueSHA256                     string
	ProfileFile, ProfileSHA256, CorrectionsFile, CorrectionsSHA256             string
	ArchiveConfig, OutputDirectory, ResourceSample, NativeBinary, NativeSHA256 string
}

func fail() {
	fmt.Fprintln(os.Stderr, "private acceleration candidate refused or incomplete; evidence retained")
	os.Exit(1)
}
func bounded(path string, max int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > max {
		return nil, fmt.Errorf("invalid private input")
	}
	return os.ReadFile(path)
}
func members(path, digest string) ([]string, error) {
	b, err := bounded(path, 65536)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(b)
	if len(digest) != 64 || hex.EncodeToString(hash[:]) != digest {
		return nil, fmt.Errorf("membership digest mismatch")
	}
	return strings.Fields(string(b)), nil
}
func main() {
	path := flag.String("config", "", "private reviewed immutable candidate config")
	validateOnly := flag.Bool("validate-only", false, "validate exact inputs and pinned native before any DB connection or execution")
	flag.Parse()
	b, err := bounded(*path, 32768)
	if err != nil {
		fail()
	}
	var c config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.Decode(new(any)) != io.EOF {
		fail()
	}
	canonical, err := members(c.CanonicalFile, c.CanonicalSHA256)
	if err != nil {
		fail()
	}
	roots, err := members(c.QueueFile, c.QueueSHA256)
	if err != nil {
		fail()
	}
	profile, err := taxapi.LoadCommissionPrivateProfile(c.ProfileFile, c.ProfileSHA256)
	if err != nil {
		fail()
	}
	corrections, err := taxapi.LoadCommissionSourceCorrections(c.CorrectionsFile, c.CorrectionsSHA256)
	if err != nil {
		fail()
	}
	from, err := time.Parse(time.DateOnly, c.From)
	if err != nil {
		fail()
	}
	asOf, err := time.Parse(time.DateOnly, c.AsOf)
	if err != nil {
		fail()
	}
	target := taxapi.CommissionFlowBuildOptions{Version: c.Version, Build: c.Build, AsOf: asOf, PrivateProfile: profile, SourceCorrections: corrections}
	if c.Chain != "mainnet" || !from.Equal(profile.From) || taxapi.ValidateCommissionCaptureCandidate(target, c.SourceVersions, canonical, roots, taxapi.CommissionCaptureCandidateOptions{ComputeOnly: c.ComputeOnly}) != nil || !filepath.IsAbs(c.OutputDirectory) || !filepath.IsAbs(c.ResourceSample) || validateNative(c.NativeBinary, c.NativeSHA256) != nil {
		fail()
	}
	if *validateOnly {
		fmt.Println("private candidate configuration validated; no database or native execution")
		return
	}
	for _, key := range requiredDBEnvironment() {
		if os.Getenv(key) == "" {
			fail()
		}
	}
	port := os.Getenv("DB_PORT")
	if port == "" {
		port = "5432"
	}
	db, err := connectDatabase(os.Getenv("DB_HOST"), port, os.Getenv("DB_NAME"), os.Getenv("DB_USER"), os.Getenv("DB_PASS"))
	if err != nil {
		fail()
	}
	pool, err := db.DB()
	if err != nil {
		fail()
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	args := []string{"--commission-flows", "--chain=" + c.Chain, "--commission-flows-from=" + c.From, "--commission-as-of=" + c.AsOf, "--commission-output-version=" + c.Version, "--commission-build=" + c.Build, "--commission-source-corrections=" + c.CorrectionsFile, "--commission-source-corrections-sha256=" + c.CorrectionsSHA256, "--commission-private-profile=" + c.ProfileFile, "--commission-private-profile-sha256=" + c.ProfileSHA256, "--commission-read-snapshot-timeout=60s", "--commission-archive-config=" + c.ArchiveConfig}
	srv := taxapi.NewServer(db, nil, taxapi.DefaultNativeAsset)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err = srv.RunCommissionCaptureCandidate(ctx, target, c.SourceVersions, canonical, roots, c.Chain, from, c.OutputDirectory, c.ResourceSample, func(ctx context.Context, root string) error {
		return taxapi.RunPinnedCommissionRoot(ctx, c.NativeBinary, c.NativeSHA256, args, root, c.OutputDirectory, func(application string) error {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var results []bool
			return db.WithContext(cancelCtx).Raw("SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE application_name = ? AND backend_type = 'client backend' AND pid <> pg_backend_pid()", application).Scan(&results).Error
		})
	}, taxapi.CommissionCaptureCandidateOptions{ComputeOnly: c.ComputeOnly})
	if err != nil {
		fail()
	}
	fmt.Println("private candidate queue complete; financial_acceptance=false public_promotion=false")
}

// A password is optional input, not an authentication-policy bypass. The
// server still decides which authentication is required. Existing callers
// supplying DB_PASS retain the same behavior; no fallback value is invented.
func requiredDBEnvironment() []string { return []string{"DB_HOST", "DB_NAME", "DB_USER"} }

func validateNative(path, digest string) error {
	if !filepath.IsAbs(path) || len(digest) != 64 {
		return fmt.Errorf("invalid native identity")
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 256<<20 || st.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("invalid native executable")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("native executable unreadable")
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != digest {
		return fmt.Errorf("native executable digest mismatch")
	}
	return nil
}
