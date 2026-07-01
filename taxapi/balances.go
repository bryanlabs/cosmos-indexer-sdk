package taxapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BalanceSnapshot is a point-in-time (daily) record of an address's ATOM
// holdings, captured live from the node so historical lookups don't need a node.
// Amounts are in ATOM (already divided by 1e6). One row per (address, date).
// (Ported from the legacy cosmos-tax-cli so the SDK can fully replace it.)
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
		Columns:   []clause.Column{{Name: "address"}, {Name: "date"}},
		DoUpdates: clause.AssignmentColumns([]string{"liquid", "staked", "reward", "height"}),
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

const (
	uatomToAtom = 1_000_000.0
	denomUatom  = "uatom"
)

type atomHoldings struct{ Liquid, Staked, Reward float64 }

type balCoin struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

// balHTTP bounds live node reads; the default client has no timeout and would
// hang a request indefinitely on a stuck node.
var balHTTP = &http.Client{Timeout: 15 * time.Second}

func balGetJSON(url string, out interface{}) error {
	resp, err := balHTTP.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d for %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// getAtomHoldings reads liquid bank balance, bonded delegations, and unclaimed
// rewards for an address from a node REST endpoint (host).
func getAtomHoldings(host, address string) (atomHoldings, error) {
	var h atomHoldings

	var bal struct {
		Balances []balCoin `json:"balances"`
	}
	if err := balGetJSON(fmt.Sprintf("%s/cosmos/bank/v1beta1/balances/%s?pagination.limit=1000", host, address), &bal); err != nil {
		return h, err
	}
	for _, c := range bal.Balances {
		if c.Denom == denomUatom {
			if v, err := strconv.ParseFloat(c.Amount, 64); err == nil {
				h.Liquid = v / uatomToAtom
			}
		}
	}

	var del struct {
		DelegationResponses []struct {
			Balance balCoin `json:"balance"`
		} `json:"delegation_responses"`
	}
	if err := balGetJSON(fmt.Sprintf("%s/cosmos/staking/v1beta1/delegations/%s?pagination.limit=1000", host, address), &del); err != nil {
		return h, err
	}
	var staked float64
	for _, d := range del.DelegationResponses {
		if d.Balance.Denom == denomUatom {
			if v, err := strconv.ParseFloat(d.Balance.Amount, 64); err == nil {
				staked += v
			}
		}
	}
	h.Staked = staked / uatomToAtom

	var rew struct {
		Total []balCoin `json:"total"`
	}
	if err := balGetJSON(fmt.Sprintf("%s/cosmos/distribution/v1beta1/delegators/%s/rewards", host, address), &rew); err != nil {
		return h, err
	}
	for _, c := range rew.Total {
		if c.Denom == denomUatom {
			if v, err := strconv.ParseFloat(c.Amount, 64); err == nil {
				h.Reward = v / uatomToAtom
			}
		}
	}
	return h, nil
}

// handleBalance serves pre-indexed (and live-fetched) ATOM holdings for an
// address. Matches the legacy cosmos-tax-cli /balance contract so the mono-app's
// TAX_API_URL can point here instead of the cli.
//
//	GET /balance?address=cosmos1...            -> latest (live from node, persisted)
//	GET /balance?address=...&date=YYYY-MM-DD   -> holdings as of a date (from snapshots)
//	GET /balance?address=...&history=true      -> full daily series
//
// Node REST endpoint for live reads comes from NODE_REST_API.
func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	address := strings.TrimSpace(q.Get("address"))
	if address == "" {
		http.Error(w, "address is required", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	if q.Get("history") == "true" {
		rows := GetBalanceSnapshotHistory(s.db, address)
		series := make([]map[string]any, 0, len(rows))
		for _, b := range rows {
			series = append(series, map[string]any{
				"date": b.Date.UTC().Format("2006-01-02"), "liquid": b.Liquid,
				"staked": b.Staked, "reward": b.Reward, "total": b.Liquid + b.Staked + b.Reward,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"address": address, "symbol": "ATOM", "history": series})
		return
	}

	// "As of" a past date: serve from snapshots only (can't historically re-fetch).
	var asOf time.Time
	if d := q.Get("date"); d != "" {
		if t, err := time.Parse("2006-01-02", d); err == nil {
			asOf = t
		}
	}
	if !asOf.IsZero() {
		if b, ok := GetBalanceSnapshotAsOf(s.db, address, asOf); ok {
			writeBalance(w, address, b, false)
			return
		}
		http.Error(w, "no balance snapshot for this address on or before that date", http.StatusNotFound)
		return
	}

	// Latest: always read live from the node (current), persisting today's row so
	// the history series accrues. Fall back to the newest cached snapshot if the
	// node is unreachable.
	if node := os.Getenv("NODE_REST_API"); node != "" {
		if h, err := getAtomHoldings(node, address); err == nil {
			today := nowUTC().Truncate(24 * time.Hour)
			b := BalanceSnapshot{Address: address, Date: today, Liquid: h.Liquid, Staked: h.Staked, Reward: h.Reward}
			_ = UpsertBalanceSnapshot(s.db, &b)
			writeBalance(w, address, b, true)
			return
		}
	}
	if b, ok := GetBalanceSnapshotAsOf(s.db, address, time.Time{}); ok {
		writeBalance(w, address, b, false)
		return
	}
	http.Error(w, "no balance available (node unreachable and no snapshot yet)", http.StatusNotFound)
}

func writeBalance(w http.ResponseWriter, address string, b BalanceSnapshot, live bool) {
	out := map[string]any{
		"address": address, "symbol": "ATOM", "date": b.Date.UTC().Format("2006-01-02"),
		"liquid": b.Liquid, "staked": b.Staked, "reward": b.Reward,
		"total": b.Liquid + b.Staked + b.Reward,
	}
	if live {
		out["live"] = true
	}
	_ = json.NewEncoder(w).Encode(out)
}
