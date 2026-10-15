package taxapi

import (
	"net/http"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// ChainReleaseCheck is one run of the release watcher (taxwatch): what the
// node reports, what GitHub publishes, whether an upgrade is scheduled, and
// whether anything needs a human or agent to review it.
type ChainReleaseCheck struct {
	ID                   uint      `gorm:"primaryKey" json:"id"`
	CheckedAt            time.Time `gorm:"index" json:"checked_at"`
	NodeVersion          string    `json:"node_version"`
	NodeSDKVersion       string    `json:"node_cosmos_sdk_version"`
	NodeHeight           int64     `json:"node_height"`
	UpstreamLatest       string    `json:"upstream_latest"`
	UpstreamLatestDate   string    `json:"upstream_latest_date"`
	UpstreamPrerelease   string    `json:"upstream_prerelease,omitempty"`
	PendingUpgradeName   string    `json:"pending_upgrade_name,omitempty"`
	PendingUpgradeHeight int64     `json:"pending_upgrade_height,omitempty"`
	UnreviewedReleases   string    `json:"unreviewed_releases"`
	Attention            bool      `json:"attention"`
	Notes                string    `json:"notes"`
}

// GET /chain/releases: the reviewed release registry plus the latest watcher
// run. attention=true means a release exists upstream or on the node that the
// tax layer has not been reviewed against.
func (s *Server) handleChainReleases(w http.ResponseWriter, _ *http.Request) {
	var last ChainReleaseCheck
	found := s.db.Order("checked_at desc").Limit(1).Find(&last).RowsAffected > 0
	resp := map[string]any{
		"latest_reviewed":  tax.LatestKnownGaiaRelease(),
		"releases":         tax.GaiaReleases,
		"upcoming_changes": tax.UpcomingChainChanges,
		"watcher":          nil,
		"attention":        false,
	}
	if found {
		resp["watcher"] = last
		resp["attention"] = last.Attention
		resp["stale"] = time.Since(last.CheckedAt) > 36*time.Hour
	}
	writeJSON(w, resp)
}
