//go:build ops818harness

package taxapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type effectiveProofRecorder struct {
	logger.Interface
	query string
}

func (r *effectiveProofRecorder) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	query, _ := fc()
	if strings.Contains(query, ") attributed ORDER BY") {
		r.query = query
	}
}

// Explicit private fixture paths and read-only connection are required. Never
// log raw source values or SQL, including assertion differences.
func TestEffectiveFeesActual43ReadOnly(t *testing.T) {
	dsn := os.Getenv("EFFECTIVE_SOURCE_DSN")
	if dsn == "" {
		t.Skip("private source proof not configured")
	}
	root := os.Getenv("EFFECTIVE_CAPTURE_ROOT")
	ref := os.Getenv("EFFECTIVE_REFERENCE_ROOT")
	output := os.Getenv("EFFECTIVE_PROOF_OUTPUT")
	recorder := &effectiveProofRecorder{Interface: logger.Default.LogMode(logger.Silent)}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: recorder})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	files, err := filepath.Glob(filepath.Join(ref, "*.rows.private.json"))
	require.NoError(t, err)
	require.Len(t, files, 43)
	type referenceRow struct {
		TxID     uint `json:"tx_id"`
		Hash     string
		Code     int64
		Denom    string
		Amount   string
		Resolved bool
		Charged  string
		Time     time.Time `json:"time_stamp"`
	}
	type capture struct {
		Address string
		Since   time.Time
		End     time.Time `json:"asof_exclusive"`
	}
	type proof struct {
		Index       int    `json:"index"`
		Rows        int    `json:"rows"`
		Millis      int64  `json:"millis"`
		Exact       bool   `json:"exact"`
		Readonly    bool   `json:"readonly"`
		InputExact  bool   `json:"input_exact"`
		Wallets     int    `json:"wallets"`
		Events      int    `json:"events"`
		InputSHA256 string `json:"nonfee_input_sha256,omitempty"`
	}
	var proofs []proof
	var plans []json.RawMessage
	for i, file := range files {
		key := filepath.Base(file)
		key = key[:len(key)-len(".rows.private.json")]
		captures, err := filepath.Glob(filepath.Join(root, "*", key+".native-cash.private.json"))
		require.NoError(t, err)
		require.Len(t, captures, 1)
		var c capture
		b, err := os.ReadFile(captures[0])
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &c))
		var rr []referenceRow
		b, err = os.ReadFile(file)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &rr))
		var want []feeRec
		seen := map[uint]bool{}
		for _, r := range rr {
			if r.Code == 0 {
				want = append(want, feeRec{Amount: r.Amount, Denom: r.Denom, Ts: r.Time, Hash: r.Hash})
				continue
			}
			if seen[r.TxID] {
				continue
			}
			seen[r.TxID] = true
			require.True(t, r.Resolved, "reference must resolve failed charges")
			coins, err := effectiveChargedCoins(r.Charged)
			require.NoError(t, err)
			for _, coin := range coins {
				want = append(want, feeRec{Amount: coin.Amount, Denom: coin.Denom, Ts: r.Time, Hash: r.Hash})
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		tx := db.WithContext(ctx).Begin()
		require.NoError(t, tx.Error)
		require.NoError(t, tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY").Error)
		started := time.Now()
		got, err := (&Server{db: tx, readOnlyLedger: true}).walletFees(apiMainnet, c.Address, c.Since, c.End)
		elapsed := time.Since(started)
		query := recorder.query
		if err != nil {
			tx.Rollback()
			cancel()
			t.Fatalf("source fee proof index %d failed (details withheld)", i)
		}
		keyfn := func(r feeRec) string {
			return fmt.Sprintf("%s/%s/%s/%s", r.Ts.UTC().Format(time.RFC3339Nano), r.Hash, r.Denom, r.Amount)
		}
		keys := func(rs []feeRec) []string {
			k := make([]string, len(rs))
			for j, r := range rs {
				k[j] = keyfn(r)
			}
			sort.Strings(k)
			return k
		}
		a, z := keys(got), keys(want)
		exact := len(a) == len(z)
		if exact {
			for j := range a {
				if a[j] != z[j] {
					exact = false
					break
				}
			}
		}
		var plan string
		require.NotEmpty(t, query)
		require.NoError(t, tx.Raw("EXPLAIN (FORMAT JSON) "+query).Row().Scan(&plan))
		plans = append(plans, json.RawMessage(plan))
		encodedPlans, e := json.MarshalIndent(plans, "", "  ")
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(output+".plans.private.json", encodedPlans, 0600))
		var readonly bool
		require.NoError(t, tx.Raw("SELECT txid_current_if_assigned() IS NULL").Scan(&readonly).Error)
		require.NoError(t, tx.Rollback().Error)
		cancel()
		pr := proof{Index: i, Rows: len(got), Millis: elapsed.Milliseconds(), Exact: exact, Readonly: readonly}
		if os.Getenv("EFFECTIVE_FULL_INPUTS") == "1" {
			var old reportInput
			b, e := os.ReadFile(strings.TrimSuffix(captures[0], ".native-cash.private.json") + ".input.private.json")
			require.NoError(t, e)
			require.NoError(t, json.Unmarshal(b, &old))
			var budget time.Duration
			if value := os.Getenv("EFFECTIVE_NATIVE_READ_TIMEOUT"); value != "" {
				budget, e = time.ParseDuration(value)
				require.NoError(t, e)
			}
			input, e := (&Server{db: db, readOnlyLedger: true, privateReadSnapshotTimeout: budget}).readReportInput(apiMainnet, []string{c.Address}, c.Since, c.End, false)
			if e != nil {
				receipt, marshalErr := json.Marshal(map[string]any{"index": i, "error": e.Error()})
				require.NoError(t, marshalErr)
				require.NoError(t, os.WriteFile(output+".error.private.json", receipt, 0600))
				t.Fatalf("native whole-input proof index %d failed (private receipt retained)", i)
			}
			pr.Wallets = len(input.Wallets)
			require.Len(t, input.Wallets, len(old.Wallets))
			require.Len(t, input.Wallets, 1)
			pr.Events = len(input.Wallets[0].Events)
			a, z = keys(input.Wallets[0].Fees), keys(want)
			require.Equal(t, len(a), len(z))
			for j := range a {
				require.True(t, a[j] == z[j], "native effective fee differs at fixed index %d", i)
			}
			input.Wallets[0].Fees = nil
			old.Wallets[0].Fees = nil
			before, e := json.Marshal(old)
			require.NoError(t, e)
			after, e := json.Marshal(input)
			require.NoError(t, e)
			pr.InputExact = bytes.Equal(before, after)
			pr.InputSHA256 = fmt.Sprintf("%x", sha256.Sum256(after))
			require.True(t, pr.InputExact, "native non-fee source input differs at fixed index %d", i)
		}
		proofs = append(proofs, pr)
		encoded, err := json.MarshalIndent(proofs, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(output, encoded, 0600))
		require.True(t, exact, "source fee proof differs at fixed index %d", i)
		require.True(t, readonly)
	}
	t.Logf("actual windows %d, exact all-coin fee equivalence and read-only transactions", len(proofs))
}
