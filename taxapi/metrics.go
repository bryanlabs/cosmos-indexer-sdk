package taxapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleMetrics exposes tax-indexer freshness in the Prometheus text exposition
// format (stdlib-only, no client_golang). One instance indexes one chain, so the
// metrics are unlabeled; Prometheus adds the pod/instance labels on scrape.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var res struct {
		MaxHeight int64
		MaxTime   *time.Time
		NumBlocks int64
	}
	// Best-effort: on error the values stay zero and the endpoint still responds.
	_ = s.db.Raw("SELECT MAX(height) AS max_height, MAX(time_stamp) AS max_time, COUNT(*) AS num_blocks FROM blocks").Scan(&res).Error

	var b strings.Builder
	fmt.Fprintf(&b, "# HELP taxindexer_latest_block_height Highest block height indexed\n# TYPE taxindexer_latest_block_height gauge\ntaxindexer_latest_block_height %d\n", res.MaxHeight)
	fmt.Fprintf(&b, "# HELP taxindexer_blocks_indexed Total blocks indexed\n# TYPE taxindexer_blocks_indexed gauge\ntaxindexer_blocks_indexed %d\n", res.NumBlocks)

	age := -1.0
	ts := 0.0
	if res.MaxTime != nil && !res.MaxTime.IsZero() {
		age = time.Since(*res.MaxTime).Seconds()
		ts = float64(res.MaxTime.Unix())
	}
	fmt.Fprintf(&b, "# HELP taxindexer_latest_block_time_seconds Unix time of the newest indexed block\n# TYPE taxindexer_latest_block_time_seconds gauge\ntaxindexer_latest_block_time_seconds %.0f\n", ts)
	fmt.Fprintf(&b, "# HELP taxindexer_data_age_seconds Seconds since the newest indexed block's time (-1 if none)\n# TYPE taxindexer_data_age_seconds gauge\ntaxindexer_data_age_seconds %.0f\n", age)

	_, _ = w.Write([]byte(b.String()))
}
