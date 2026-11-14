package taxapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func privateProfileFixture(t *testing.T) (*CommissionPrivateProfile, string) {
	t.Helper()
	root := os.Getenv("COMMISSION_PROFILE_TEST_ROOT")
	if root == "" {
		t.Skip("private source-bound fixture not configured")
	}
	b, err := os.ReadFile(filepath.Join(root, "profile.private.json"))
	require.NoError(t, err)
	p, err := LoadCommissionPrivateProfile(filepath.Join(root, "profile.private.json"), privateSHA(b))
	require.NoError(t, err)
	return p, root
}
func TestCommissionPrivateProfileSourceAndNegativeControls(t *testing.T) {
	p, root := privateProfileFixture(t)
	b, err := os.ReadFile(filepath.Join(root, "source-seal.private.json"))
	require.NoError(t, err)
	require.Equal(t, privateSourceSealSHA, privateSHA(b))
	require.NoError(t, validatePrivateSourceParse(b))
	for _, field := range []string{"message_bytes", "message_type", "error", "tx", "tax_rows", "events"} {
		t.Run(field, func(t *testing.T) {
			var m map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(b, &m))
			m[field] = json.RawMessage(`null`)
			if field == "events" {
				m[field] = json.RawMessage(`[{"type":"coin_spent","attributes":[]}]`)
			}
			if field == "tax_rows" {
				m[field] = json.RawMessage(`[{}]`)
			}
			changed, e := json.Marshal(m)
			require.NoError(t, e)
			require.Error(t, validatePrivateSourceParse(changed))
		})
	}
	for _, field := range []string{"source_sha256", "parser_sha256", "economic_proof_sha256", "from", "as_of", "schema"} {
		t.Run("profile_"+field, func(t *testing.T) {
			b, e := os.ReadFile(filepath.Join(root, "profile.private.json"))
			require.NoError(t, e)
			var m map[string]any
			require.NoError(t, json.Unmarshal(b, &m))
			m[field] = "wrong"
			b, e = json.Marshal(m)
			require.NoError(t, e)
			path := filepath.Join(t.TempDir(), "profile")
			require.NoError(t, os.WriteFile(path, b, 0600))
			_, e = LoadCommissionPrivateProfile(path, privateSHA(b))
			require.Error(t, e)
		})
	}
	_, err = LoadCommissionPrivateProfile(filepath.Join(root, "profile.private.json"), "wrong")
	require.Error(t, err)
	symlink := filepath.Join(t.TempDir(), "symlink")
	require.NoError(t, os.Symlink(filepath.Join(root, "profile.private.json"), symlink))
	_, err = LoadCommissionPrivateProfile(symlink, p.digest)
	require.Error(t, err)
	opts := CommissionFlowBuildOptions{Version: "new-profile" + p.digest[:12], Build: "sha256:new", AsOf: p.AsOf, PrivateProfile: p}
	require.NoError(t, opts.validate())
	opts.Version = "old-a5"
	require.Error(t, opts.validate())
	require.Error(t, (&Server{commissionPrivateProfile: p}).validatePrivateProjection())
}

func TestCommissionPrivateProfileTraceIsolation(t *testing.T) {
	p, _ := privateProfileFixture(t)
	s := &Server{commissionPrivateProfile: p, readOnlyLedger: true, privateProjectionReceipts: &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}}
	base := &dbFlowLoader{s: s, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}, private: true}
	a, la := s.privateProjectionTrace(base)
	b, lb := s.privateProjectionTrace(base)
	a.privateProjectionReceipts.retain([]string{"wallet-a"}, p.From, p.AsOf, CommissionPrivateProjectionReceipt{OriginalParserErrors: 1, ReclassifiedParserErrors: 1})
	require.Len(t, la.evidence().PrivateProjections, 1)
	require.Empty(t, lb.evidence().PrivateProjections)
	require.Empty(t, b.privateProjectionReceipts.receipts())
	require.Empty(t, s.privateProjectionReceipts.receipts())
	opts := CommissionFlowBuildOptions{Version: "new-profile" + p.digest[:12], Build: "new", AsOf: p.AsOf, PrivateProfile: p}
	run := CommissionFlowRun{Chain: apiMainnet, Validator: "validator", FromDay: p.From, AsOf: p.AsOf.AddDate(0, 0, -1), Status: flowStatusOK}
	raw, e := json.Marshal(run)
	require.NoError(t, e)
	row := CommissionFlowVersion{Version: opts.Version, Build: opts.Build, Chain: run.Chain, Validator: run.Validator, RunJSON: string(raw)}
	require.Error(t, reusableCommissionFlowVersion(row, opts, apiMainnet, run.Validator, p.From))
	row.PrivateProfileSHA256 = p.digest
	require.NoError(t, reusableCommissionFlowVersion(row, opts, apiMainnet, run.Validator, p.From))
	opts.PrivateProfile = nil
	require.Error(t, opts.validate())
	require.Error(t, reusableCommissionFlowVersion(row, opts, apiMainnet, run.Validator, p.From))
}

func TestCommissionPrivateProfileNativeSnapshot(t *testing.T) {
	if os.Getenv("COMMISSION_PROFILE_NATIVE") != "1" {
		t.Skip("actual native proof opt-in")
	}
	p, root := privateProfileFixture(t)
	b, e := os.ReadFile("/Users/danb/.cache/commission-acceleration/prepared-20261114t1500/original-job.private.json")
	require.NoError(t, e)
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct{ Name, Value string }
					}
				}
			}
		}
	}
	require.NoError(t, json.Unmarshal(b, &job))
	env := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	dsn := fmt.Sprintf("host=127.0.0.1 port=15438 user=%s password=%s dbname=%s sslmode=disable default_transaction_read_only=on statement_timeout=10000 lock_timeout=500 application_name=commission-profile-native-ro", env["DB_USER"], env["DB_PASS"], env["DB_NAME"])
	db, e := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, e)
	conn, e := db.DB()
	require.NoError(t, e)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	b, e = os.ReadFile("/Users/danb/.cache/readiness-cause-20261114T1646/failure-response.private.json")
	require.NoError(t, e)
	var failures []struct {
		Evidence struct {
			Failure *CommissionReadFailure `json:"ReadFailure"`
		} `json:"evidence_json"`
	}
	require.NoError(t, json.Unmarshal(b, &failures))
	var f *CommissionReadFailure
	for _, v := range failures {
		if v.Evidence.Failure != nil && v.Evidence.Failure.PhaseElapsedMillis == 19 {
			f = v.Evidence.Failure
		}
	}
	require.NotNil(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	journal := &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}
	s := &Server{db: db.WithContext(ctx), readOnlyLedger: true, privateReadSnapshotTimeout: 60 * time.Second, commissionPrivateProfile: p, privateProjectionReceipts: journal}
	var snapshot string
	e = s.withReadSnapshot(func(r *Server) error {
		original := &Server{db: r.db, readOnlyLedger: true, readSnapshotStarted: r.readSnapshotStarted}
		before, e := original.classificationReadiness(apiMainnet, []string{f.Address}, f.Start, f.End)
		if e != nil {
			return e
		}
		if before.ParserErrors != 1 || before.ExportReady {
			return fmt.Errorf("default no longer refuses")
		}
		projected, e := r.classificationReadiness(apiMainnet, []string{f.Address}, f.Start, f.End)
		if e != nil {
			return e
		}
		if projected.ParserErrors != 1 || projected.ReclassifiedParserErrors != 1 || projected.RemainingParserErrors != 0 || !projected.ExportReady {
			return fmt.Errorf("projection did not preserve original counts")
		}
		feesBefore, e := original.walletFees(apiMainnet, f.Address, f.Start, f.End)
		if e != nil {
			return e
		}
		feesAfter, e := r.walletFees(apiMainnet, f.Address, f.Start, f.End)
		if e != nil {
			return e
		}
		if len(feesBefore) == 0 || fmt.Sprintf("%#v", feesBefore) != fmt.Sprintf("%#v", feesAfter) {
			return fmt.Errorf("outer fee read changed")
		}
		if e := r.validatePrivateProjection(); e != nil {
			return e
		}
		snapshot = r.privateProjectionSnapshot
		unknown, e := r.classificationReadiness(apiTestnet, []string{f.Address}, f.Start, f.End)
		_ = unknown
		if e == nil {
			return fmt.Errorf("wrong chain accepted")
		}
		return nil
	})
	require.NoError(t, e)
	// Ordinary readReportInput remains refused before financial rows even though
	// this binary contains the corrected parser and the private staging path.
	ordinary := &Server{db: db.WithContext(ctx)}
	input, e := ordinary.readReportInput(apiMainnet, []string{f.Address}, f.Start, f.End, false)
	var refused reportReadinessRequired
	require.True(t, errors.As(e, &refused))
	require.EqualValues(t, 1, refused.Readiness.ParserErrors)
	require.Empty(t, input.Wallets)
	receipts := journal.receipts()
	require.NotEmpty(t, receipts)
	rolledBack := &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}
	s.privateProjectionReceipts = rolledBack
	e = s.withReadSnapshot(func(r *Server) error {
		_, err := r.classificationReadiness(apiMainnet, []string{f.Address}, f.Start, f.End)
		if err != nil {
			return err
		}
		return context.Canceled
	})
	require.ErrorIs(t, e, context.Canceled)
	require.Empty(t, rolledBack.receipts())
	out := map[string]any{"private_projection_pass": true, "same_readonly_snapshot": snapshot, "original_parser_errors": 1, "reclassified_parser_errors": 1, "remaining_parser_errors": 0, "outer_fee_reader_unchanged": true, "default_refuses": true, "original_source_unchanged": true, "financial_acceptance": false, "profile_sha256": p.digest, "source_sha256": p.SourceSHA256, "at": time.Now().UTC()}
	data, e := json.Marshal(out)
	require.NoError(t, e)
	receipt := os.Getenv("COMMISSION_PROFILE_RECEIPT")
	if receipt == "" {
		receipt = filepath.Join(root, "native-profile.safe.json")
	}
	file, e := os.OpenFile(receipt, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require.NoError(t, e)
	_, e = file.Write(data)
	require.NoError(t, e)
	require.NoError(t, file.Close())
}
