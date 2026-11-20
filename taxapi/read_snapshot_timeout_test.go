package taxapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type snapshotSQLStateError string

func (e snapshotSQLStateError) Error() string    { return "unsafe driver wallet text" }
func (e snapshotSQLStateError) SQLState() string { return string(e) }

func TestPrivateReadFailureCause(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("wrapped: %w", snapshotSQLStateError("42P01")), "sqlstate_42P01"},
		{snapshotSQLStateError("unsafe wallet text"), "other"},
		{errors.New("unsafe driver wallet text"), "other"},
	} {
		p := (&Server{readOnlyLedger: true}).privateReadPhases([]string{"privatewallet"}, time.Now(), time.Now(), "fees")
		err := p.wrap(tc.err)
		var failure *CommissionReadFailure
		require.ErrorAs(t, err, &failure)
		require.Equal(t, tc.want, failure.Cause)
		require.Contains(t, failure.Error(), "cause="+tc.want)
		require.NotContains(t, failure.Error(), "unsafe")
		data, marshalErr := json.Marshal(failure)
		require.NoError(t, marshalErr)
		var stored CommissionReadFailure
		require.NoError(t, json.Unmarshal(data, &stored))
		require.Equal(t, tc.want, stored.Cause)
	}
}

func TestReadSnapshotDeadlinePrivateOnly(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		private                bool
		override, caller, want time.Duration
	}{
		{"default_public", false, 0, 0, 30 * time.Second},
		{"public_ignores_override", false, time.Minute, 0, 30 * time.Second},
		{"private_default", true, 0, 0, 30 * time.Second},
		{"private_override", true, time.Minute, 0, time.Minute},
		{"private_caller_shorter", true, time.Minute, time.Second, time.Second},
		{"public_caller_shorter", false, 0, time.Second, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := context.Background()
			if tc.caller > 0 {
				var cancel context.CancelFunc
				parent, cancel = context.WithTimeout(parent, tc.caller)
				defer cancel()
			}
			s := &Server{db: &gorm.DB{Statement: &gorm.Statement{Context: parent}}, readOnlyLedger: tc.private, privateReadSnapshotTimeout: tc.override}
			began := time.Now()
			ctx, cancel := s.readSnapshotContext()
			defer cancel()
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.InDelta(t, tc.want.Seconds(), deadline.Sub(began).Seconds(), 0.1)
		})
	}
}

func TestCommissionReadTimeoutOptionValidation(t *testing.T) {
	o := CommissionFlowBuildOptions{Version: "proof", Build: "source", AsOf: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
	for _, timeout := range []time.Duration{0, time.Minute} {
		o.ReadSnapshotTimeout = timeout
		require.NoError(t, o.validate())
	}
	o.ReadSnapshotTimeout = -time.Nanosecond
	require.Error(t, o.validate())
}

func TestPrivateReadFailureSafeWrappingAndEvidence(t *testing.T) {
	address := "cosmos1privatewallet"
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	cause := fmt.Errorf("unsafe driver text %s: %w", address, context.DeadlineExceeded)
	public := (&Server{}).privateReadPhases([]string{address}, start, end, "fees").wrap(cause)
	require.Same(t, cause, public)
	s := &Server{readOnlyLedger: true, readSnapshotStarted: time.Now().Add(-time.Second)}
	phases := s.privateReadPhases([]string{address}, start, end, "tax_events")
	phases.next("fees")
	err := phases.wrap(cause)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotContains(t, err.Error(), address)
	require.NotContains(t, err.Error(), "unsafe driver")
	require.Contains(t, err.Error(), fmt.Sprintf("%x", sha256.Sum256([]byte(address))))
	var failure *CommissionReadFailure
	require.True(t, errors.As(err, &failure))
	require.Equal(t, address, failure.Address)
	require.Equal(t, "fees", failure.Phase)
	require.Equal(t, "deadline_exceeded", failure.Cause)
	require.GreaterOrEqual(t, failure.TotalElapsedMillis, int64(1000))
	require.Equal(t, start, failure.Start)
	require.Equal(t, end, failure.End)
	require.Contains(t, failure.PhaseMillis, "tax_events")
	require.Same(t, err, phases.wrap(err))
	evidence := CommissionFlowEvidence{}
	evidence.retainReadFailure(fmt.Errorf("trace: %w", err), []*flowWallet{{Addr: "first", Since: start, Hop: 0}, {Addr: "second", Since: start, Hop: 1}})
	require.Same(t, failure, evidence.ReadFailure)
	require.Equal(t, []CommissionFlowReadPrefix{{Address: "first", Since: start, Hop: 0}, {Address: "second", Since: start, Hop: 1}}, evidence.ReadPrefix)
	b, jerr := json.Marshal(evidence)
	require.NoError(t, jerr)
	require.Contains(t, string(b), address)
	require.NotContains(t, string(b), "unsafe driver")
	var roundtrip CommissionFlowEvidence
	require.NoError(t, json.Unmarshal(b, &roundtrip))
	require.Equal(t, address, roundtrip.ReadFailure.Address)
	require.Equal(t, failure.PhaseMillis, roundtrip.ReadFailure.PhaseMillis)
}

// Optional native PostgreSQL proof, using only an explicitly supplied test DB.
func TestReadSnapshotFacadePostgres(t *testing.T) {
	dsn := os.Getenv("COSMOS_TAX_VERSIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("set COSMOS_TAX_VERSIONS_TEST_DSN to a dedicated test database")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	conn, err := db.DB()
	require.NoError(t, err)
	defer conn.Close()
	for _, private := range []bool{false, true} {
		s := &Server{db: db, readOnlyLedger: private, privateReadSnapshotTimeout: time.Minute}
		began := time.Now()
		err = s.withReadSnapshot(func(reader *Server) error {
			require.Equal(t, private, reader.readOnlyLedger)
			require.Equal(t, time.Minute, reader.privateReadSnapshotTimeout)
			deadline, ok := reader.db.Statement.Context.Deadline()
			require.True(t, ok)
			want := 30 * time.Second
			if private {
				want = time.Minute
			}
			require.InDelta(t, want.Seconds(), deadline.Sub(began).Seconds(), 0.2)
			var readOnly string
			require.NoError(t, reader.db.Raw("SHOW transaction_read_only").Scan(&readOnly).Error)
			require.Equal(t, "on", readOnly)
			return context.Canceled
		})
		require.ErrorIs(t, err, context.Canceled)
	}
}
