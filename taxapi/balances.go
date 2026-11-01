package taxapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BalanceSnapshot is a point-in-time (daily) record of an address's native-
// asset holdings, captured live from the node so historical lookups don't
// need a node. Amounts are display units (already divided by 10^decimals for
// this deployment's NativeAsset, see server.go — NOT hardcoded to ATOM,
// INF-208). One row per (address, date). (Ported from the legacy
// cosmos-tax-cli so the SDK can fully replace it.)
type BalanceSnapshot struct {
	ID      uint
	Address string    `gorm:"index:idx_bal_addr_date,priority:1,unique"`
	Date    time.Time `gorm:"index:idx_bal_addr_date,priority:2,unique"`
	Liquid  float64   // spendable bank balance
	Staked  float64   // bonded delegations
	Reward  float64   // unclaimed staking rewards
	Height  int64
}

// UpsertBalanceSnapshot writes (or replaces) the snapshot for an address+date.
func UpsertBalanceSnapshot(db *gorm.DB, s *BalanceSnapshot) error {
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: apiAddress}, {Name: apiDate}},
		DoUpdates: clause.AssignmentColumns([]string{apiLiquid, apiStaked, apiReward, "height"}),
	}).Create(s).Error
}

// GetBalanceSnapshotAsOf returns the most recent snapshot at or before asOf (the
// holdings "as of" that date), or the latest if asOf is zero. False if none.
func GetBalanceSnapshotAsOf(db *gorm.DB, address string, asOf time.Time) (BalanceSnapshot, bool) {
	var s BalanceSnapshot
	q := db.Where("address = ?", address)
	if !asOf.IsZero() {
		q = q.Where("date <= ?", asOf)
	}
	res := q.Order("date desc").Limit(1).Find(&s)
	return s, res.RowsAffected > 0
}

// GetBalanceSnapshotHistory returns all snapshots for an address ordered by date.
func GetBalanceSnapshotHistory(db *gorm.DB, address string) []BalanceSnapshot {
	var rows []BalanceSnapshot
	db.Where("address = ?", address).Order("date asc").Find(&rows)
	return rows
}

// --- live node read (ported from cosmos-tax-cli/rest/balances.go) ---

type nativeHoldings struct{ Liquid, Staked, Reward float64 }

type balCoin struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

func balGetJSON(origin, path string, query url.Values, out any) error {
	return configuredJSONGet(&http.Client{Timeout: 8 * time.Second}, origin, path, query, out)
}

// getNativeHoldings reads liquid bank balance, bonded delegations, and
// unclaimed rewards for an address from a node REST endpoint (host), for
// this deployment's native denom (e.g. uatom, uusdc — see NativeAsset,
// INF-208). Chains with no meaningful staking module for typical holders
// (e.g. Noble) simply report Staked/Reward as 0; the delegation/rewards
// queries 404 gracefully rather than failing the whole lookup.
func getNativeHoldings(host, address string, native NativeAsset) (nativeHoldings, error) {
	var h nativeHoldings
	unitsPerDisplay := math.Pow10(native.Decimals)

	var bal struct {
		Balances []balCoin `json:"balances"`
	}
	if err := balGetJSON(host, "/cosmos/bank/v1beta1/balances/"+address, url.Values{apiPaginationLimit: {"1000"}}, &bal); err != nil {
		return h, err
	}
	for _, c := range bal.Balances {
		if c.Denom == native.Denom {
			if v, err := strconv.ParseFloat(c.Amount, 64); err == nil {
				h.Liquid = v / unitsPerDisplay
			}
		}
	}

	var del struct {
		DelegationResponses []struct {
			Balance balCoin `json:"balance"`
		} `json:"delegation_responses"`
	}
	if err := balGetJSON(host, "/cosmos/staking/v1beta1/delegations/"+address, url.Values{apiPaginationLimit: {"1000"}}, &del); err == nil {
		var staked float64
		for _, d := range del.DelegationResponses {
			if d.Balance.Denom == native.Denom {
				if v, err := strconv.ParseFloat(d.Balance.Amount, 64); err == nil {
					staked += v
				}
			}
		}
		h.Staked = staked / unitsPerDisplay
	}

	var rew struct {
		Total []balCoin `json:"total"`
	}
	if err := balGetJSON(host, "/cosmos/distribution/v1beta1/delegators/"+address+"/rewards", nil, &rew); err == nil {
		for _, c := range rew.Total {
			if c.Denom == native.Denom {
				if v, err := strconv.ParseFloat(c.Amount, 64); err == nil {
					h.Reward = v / unitsPerDisplay
				}
			}
		}
	}
	return h, nil
}

// handleBalance serves pre-indexed (and live-fetched) native-asset holdings
// for an address. Matches the legacy cosmos-tax-cli /balance contract so the
// mono-app's TAX_API_URL can point here instead of the cli.
//
//	GET /balance?address=cosmos1...            -> latest (live from node, persisted)
//	GET /balance?address=...&date=YYYY-MM-DD   -> holdings as of a date (from snapshots)
//	GET /balance?address=...&history=true      -> full daily series
//
// Node REST endpoint for live reads comes from NODE_REST_API.
func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	address := strings.TrimSpace(q.Get(apiAddress))
	if address == "" {
		http.Error(w, "address is required", http.StatusBadRequest)
		return
	}
	w.Header().Set(apiContentType, apiJSONContentType)

	if q.Get("history") == apiTrue {
		rows := GetBalanceSnapshotHistory(s.db, address)
		series := make([]map[string]any, 0, len(rows))
		for _, b := range rows {
			series = append(series, map[string]any{
				apiDate: b.Date.UTC().Format(apiDateLayout), apiLiquid: b.Liquid,
				apiStaked: b.Staked, apiReward: b.Reward, apiTotal: b.Liquid + b.Staked + b.Reward,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{apiAddress: address, apiSymbol: s.native.Symbol, "history": series})
		return
	}

	// "As of" a past date: serve from snapshots only (can't historically re-fetch).
	var asOf time.Time
	if d := q.Get(apiDate); d != "" {
		if t, err := time.Parse(apiDateLayout, d); err == nil {
			asOf = t
		}
	}
	if !asOf.IsZero() {
		if b, ok := GetBalanceSnapshotAsOf(s.db, address, asOf); ok {
			s.writeBalance(w, address, b, false)
			return
		}
		http.Error(w, "no balance snapshot for this address on or before that date", http.StatusNotFound)
		return
	}

	// Latest: always read live from the node (current), persisting today's row so
	// the history series accrues. Fall back to the newest cached snapshot if the
	// node is unreachable.
	if node := os.Getenv("NODE_REST_API"); node != "" {
		if h, err := getNativeHoldings(node, address, s.native); err == nil {
			today := nowUTC().Truncate(24 * time.Hour)
			b := BalanceSnapshot{Address: address, Date: today, Liquid: h.Liquid, Staked: h.Staked, Reward: h.Reward}
			_ = UpsertBalanceSnapshot(s.db, &b)
			s.writeBalance(w, address, b, true)
			return
		}
	}
	if b, ok := GetBalanceSnapshotAsOf(s.db, address, time.Time{}); ok {
		s.writeBalance(w, address, b, false)
		return
	}
	http.Error(w, "no balance available (node unreachable and no snapshot yet)", http.StatusNotFound)
}

func (s *Server) writeBalance(w http.ResponseWriter, address string, b BalanceSnapshot, live bool) {
	out := map[string]any{
		apiAddress: address, apiSymbol: s.native.Symbol, apiDate: b.Date.UTC().Format(apiDateLayout),
		apiLiquid: b.Liquid, apiStaked: b.Staked, apiReward: b.Reward,
		apiTotal: b.Liquid + b.Staked + b.Reward,
	}
	if live {
		out["live"] = true
	}
	_ = json.NewEncoder(w).Encode(out)
}
