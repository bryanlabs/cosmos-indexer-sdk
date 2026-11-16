package hub2promotion

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPG5MeasuredSmallFixtureWALBackendMemoryAndLocks(t *testing.T) {
	f := newFixture(t)
	var lsn string
	require.NoError(t, f.owner.QueryRow("SELECT pg_current_wal_insert_lsn()::text").Scan(&lsn))
	var rssKiB, vszKiB, peakKiB int64
	r, e := merge(context.Background(), f.loader, f.owner, f.m, f.runtime, hooks{beforeCommit: func() error {
		var pid int
		require.NoError(t, f.owner.QueryRow(`SELECT pid FROM pg_stat_activity WHERE datname=$1 AND usename=$2 AND state='idle in transaction'`, f.m.Destination.Database, f.m.Scope.Loader).Scan(&pid))
		out, e := exec.Command("kubectl", "--context", "bryanlabs", "-n", "agents", "exec", helperPod(t), "--", "sh", "-c", `while IFS= read -r line; do case "$line" in VmHWM:*|VmRSS:*|VmSize:*) printf '%s\n' "$line";; esac; done < /proc/"$1"/status`, "sh", fmt.Sprint(pid)).Output()
		require.NoError(t, e)
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 {
				require.Equal(t, "kB", fields[2])
				v, e := strconv.ParseInt(fields[1], 10, 64)
				require.NoError(t, e)
				switch fields[0] {
				case "VmRSS:":
					rssKiB = v
				case "VmSize:":
					vszKiB = v
				case "VmHWM:":
					peakKiB = v
				}
			}
		}
		require.Positive(t, rssKiB)
		require.GreaterOrEqual(t, peakKiB, rssKiB)
		return nil
	}})
	require.NoError(t, e)
	var wal, size int64
	require.NoError(t, f.owner.QueryRow("SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(),$1::pg_lsn)::bigint,pg_database_size(current_database())", lsn).Scan(&wal, &size))
	require.Positive(t, wal)
	metrics := struct {
		Result                         Result `json:"result"`
		SharedServerWALDeltaUpperBound int64  `json:"shared_server_wal_delta_upper_bound_bytes"`
		BackendRSS                     int64  `json:"backend_rss_kib"`
		BackendVSZ                     int64  `json:"backend_vsz_kib"`
		BackendPeakRSS                 int64  `json:"backend_peak_rss_kib"`
		FixtureDBBytes                 int64  `json:"fixture_db_bytes"`
	}{r, wal, rssKiB, vszKiB, peakKiB, size}
	b, e := json.MarshalIndent(metrics, "", "  ")
	require.NoError(t, e)
	savePin(t, f.dir, "small-measurements.json", b)
	t.Logf("measured small fixture rows=%d lock=%s wall=%s RSS=%dKiB sharedWALupper=%dB fixtureDB=%dB", r.Rows, r.LockWait, r.Duration, rssKiB, wal, size)
}
