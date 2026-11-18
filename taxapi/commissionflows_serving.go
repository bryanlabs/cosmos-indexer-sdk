package taxapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// The commission page serves each validator's newest accepted versioned
// result and falls back to the legacy run table for validators that have
// none. A versioned result is accepted only when it comes from one of the
// reviewed collections below, carries that collection's exact build, private
// profile and window, names the validator it is stored under, and finished:
// ok, reconciled or drift. A stored error is not a result. A drift result is
// a finished trace whose rebuilt balances differ from the chain; its own page
// shows it labelled as drift and the Hub leaves it out of its totals.
//
// The API reads only the identity, run and rows columns. Evidence stays
// private and is never selected.

// commissionServingSource is one versioned collection the API may serve.
// Root, when set, limits a retry collection to the one validator whose
// sha256 it pins.
type commissionServingSource struct {
	Version string
	Build   string
	Profile string
	FromDay string
	AsOf    string
	Root    string
}

const (
	flowCorrectedBuild   = "sha256:fc277f445a4257a8855bce376c44cc71cc90a51417f72be8cba5e141d0abafd4"
	flowCorrectedProfile = "6bab7886b14ec3728c98f7e9b5e77a9b2f422a5d0fbc782178e8be65586739ce"
	flowCorrectedPrefix  = "ops818-page82a-profile53da2904e3a6-sourcesade534f8e5f7-"
	flowCorrectedFrom    = "2024-03-21"
	flowCorrectedAsOf    = "2026-11-12"
)

func flowCorrected(suffix string) commissionServingSource {
	return commissionServingSource{Version: flowCorrectedPrefix + suffix, Build: flowCorrectedBuild, Profile: flowCorrectedProfile, FromDay: flowCorrectedFrom, AsOf: flowCorrectedAsOf}
}

// flowCorrectedRoot is a corrected-build retry that may serve only its one
// pinned validator root.
func flowCorrectedRoot(suffix, root string) commissionServingSource {
	r := flowCorrected(suffix)
	r.Root = root
	return r
}

// The LSM reward retries run the native build that reads the corrections
// manifest without the six tokenize-share rewards, after hub4 heights
// 31944427 and 31944434 were re-indexed to store those rewards themselves.
// Their stored profile binds that manifest, so build, profile and version
// prefix all differ from the corrected shards.
const (
	flowLSMBuild   = "sha256:b7075f6a5b020616d7ad4b6b8f1275e6a699f10a30ff148dde7e800e4e0e56b8"
	flowLSMProfile = "dc197eef1ed664d2f53b2453a3355ac97cd052f827b2bf117cd180db25dbba68"
	flowLSMPrefix  = "ops818-page82a-profile53da2904e3a6-sources40d696172138-"
)

// flowLSMRoot is an LSM reward retry that may serve only its one pinned
// validator root.
func flowLSMRoot(suffix, root string) commissionServingSource {
	return commissionServingSource{Version: flowLSMPrefix + suffix, Build: flowLSMBuild, Profile: flowLSMProfile, FromDay: flowCorrectedFrom, AsOf: flowCorrectedAsOf, Root: root}
}

// defaultCommissionServingSources is in priority order: registered retries
// first, then the corrected shards, then the two source collections the
// shards adopt from. A validator is served from the first collection that
// holds an accepted result for it.
var defaultCommissionServingSources = map[string][]commissionServingSource{
	apiMainnet: {
		{
			Version: "ops818-unjail-profile930320029ed0-sources962174bfb014-s06-retry-20261115-v2",
			Build:   "sha256:fa71e36b2feec15498ec94b778e2571deebc1e11197a9da7b94968b658d3a874",
			Profile: "541329afb4e52d3d5f9a9503ed0851e4d66f780071630b8bfbd0dc2ab3fd46aa",
			FromDay: flowCorrectedFrom, AsOf: flowCorrectedAsOf,
			Root: "c07d4e7c6b6e288cc7c23111d04eca5c50872c5b58fdb9b5f2ec67ae2093cdad",
		},
		func() commissionServingSource {
			r := flowCorrected("accel-later-s02-20261115-deadline-retry-v2")
			r.Root = "2d473f4858d9987775254ce4bff8939f812bfefeb04eb1e6f0a5ebde72969fd8"
			return r
		}(),
		flowCorrectedRoot("accel-four-s05-20261115-deadline-retry-v4", "ab12977c1a2135f7aeea69534171ee3c2bec9a03f3650cbf5dff76caefb19345"),
		flowCorrectedRoot("accel-four-s03-20261115-deadline-retry-v3", "290eefd100eb62044e1da01618881c70ac9aa783672b9547415766c16bcf06ff"),
		flowCorrectedRoot("accel-four-s04-20261115-deadline-retry-v4", "0a11a4c0090436746a12555ec8f07a503c467e849bccf9ef550a8e2985f2b5ab"),
		flowCorrectedRoot("accel-later-s02-20261115-deadline-retry-v3", "50c56a74de37966c61278d7381538fe0d8fa3746d87eadae5d633a471f017de1"),
		flowCorrectedRoot("accel-four-s07-20261115-deadline-retry-v2", "320bcb826109bdff2fcab865b3971af8023c5d8b2ba24f02961a4faca99425aa"),
		flowCorrectedRoot("accel-later-s03-20261115-deadline-retry-v2", "001308de505138aa91ea460b148382f1dd7b6528a4a307e6333dff716f08a7ae"),
		flowCorrectedRoot("accel-four-s05-20261115-deadline-retry-v3", "5187c6ad32a6209ebd2f8b36e2f78b95950a96e046c474db04ca64b8c981f885"),
		flowCorrectedRoot("accel-four-s04-20261115-deadline-retry-v3", "6e79e2a8e3e5b77e7d96bc45c28592f36610c3992d54bb6e828ad44e375f1302"),
		flowLSMRoot("accel-four-s01-20261115-lsm-retry-v1", "bc0089661f770388eeb5acd70a947bbc9c47ab0808b6fda90df95ff57800f15a"),
		flowLSMRoot("accel-four-s06-20261115-lsm-retry-v1", "85f6d8206eebe56d592d5c82aa2978a04e7edf1ad6fcfb8165e6b15020298fd6"),
		flowCorrected("accel-four-s01-20261115-v1"),
		flowCorrected("accel-four-s02-20261115-v1"),
		flowCorrected("accel-four-s03-20261115-v1"),
		flowCorrected("accel-four-s04-20261115-v1"),
		flowCorrected("accel-four-s05-20261115-v1"),
		flowCorrected("accel-four-s06-20261115-v1"),
		flowCorrected("accel-four-s07-20261115-v1"),
		flowCorrected("accel-later-s01-20261115-v1"),
		flowCorrected("accel-later-s02-20261115-v1"),
		flowCorrected("accel-later-s03-20261115-v1"),
		flowCorrected("accel-four-s02-20261115-split-b-v2"),
		flowCorrected("accel-four-s01-20261115-split-b-v2"),
		// Validators the stopped runs never reached, each list run on an idle host in its own version.
		flowCorrected("accel-four-s06-20261115-remainder-v1"),
		flowCorrected("accel-later-s02-20261115-remainder-v1"),
		flowCorrected("accel-later-s01-20261115-remainder-v1"),
		flowCorrected("full324-four-20261115-v2"),
		flowCorrected("full324-later-two-20261115-v2"),
	},
}

func (s *Server) commissionServingSources(chain string) []commissionServingSource {
	if s.flowSources != nil {
		return s.flowSources[chain]
	}
	return defaultCommissionServingSources[chain]
}

// flowVersionColumns are the only versioned columns the API reads.
const flowVersionColumns = "version, chain, validator, build, private_profile_sha256, computed_at, run_json::text AS run_json, rows_json::text AS rows_json"

type flowVersionRow struct {
	Version              string
	Chain                string
	Validator            string
	Build                string
	PrivateProfileSHA256 string
	ComputedAt           *time.Time
	RunJSON              string
	RowsJSON             string
}

// servedCommissionFlow is the result a validator's page and the Hub use.
type servedCommissionFlow struct {
	Run CommissionFlowRun
	// Rows are the versioned flow lines; nil for a legacy run, whose lines
	// stay in the legacy flow table.
	Rows      []CommissionFlow
	Versioned bool
}

func flowValidatorRoot(validator string) string {
	sum := sha256.Sum256([]byte(validator))
	return hex.EncodeToString(sum[:])
}

func flowDateText(t time.Time) string { return t.UTC().Format(apiDateLayout) }

// acceptFlowVersion decodes row and reports whether src may serve it.
func acceptFlowVersion(src commissionServingSource, row flowVersionRow, chain string) (servedCommissionFlow, bool) {
	if row.Version != src.Version || row.Chain != chain || row.Build != src.Build || row.PrivateProfileSHA256 != src.Profile {
		return servedCommissionFlow{}, false
	}
	if src.Root != "" && flowValidatorRoot(row.Validator) != src.Root {
		return servedCommissionFlow{}, false
	}
	// A field moved to private payload chunks is not readable here.
	if isFlowPayloadMarker(row.RunJSON) || isFlowPayloadMarker(row.RowsJSON) {
		return servedCommissionFlow{}, false
	}
	var run CommissionFlowRun
	if json.Unmarshal([]byte(row.RunJSON), &run) != nil {
		return servedCommissionFlow{}, false
	}
	if run.Chain != chain || run.Validator != row.Validator || flowDateText(run.FromDay) != src.FromDay || flowDateText(run.AsOf) != src.AsOf {
		return servedCommissionFlow{}, false
	}
	switch run.Status {
	case flowStatusOK, flowStatusReconciled, flowStatusDrift:
	default:
		return servedCommissionFlow{}, false
	}
	rows := []CommissionFlow{}
	if json.Unmarshal([]byte(row.RowsJSON), &rows) != nil {
		return servedCommissionFlow{}, false
	}
	for _, f := range rows {
		if f.Chain != chain || f.Validator != row.Validator {
			return servedCommissionFlow{}, false
		}
		if _, err := decimal.NewFromString(f.AmountBase); err != nil {
			return servedCommissionFlow{}, false
		}
	}
	return servedCommissionFlow{Run: run, Rows: rows, Versioned: true}, true
}

// isFlowPayloadMarker matches the collector's chunked-payload placeholder,
// a JSON string in place of the field's object or array.
func isFlowPayloadMarker(raw string) bool {
	var value string
	return json.Unmarshal([]byte(raw), &value) == nil && strings.HasPrefix(value, "cosmos-commission-payload/")
}

// acceptedFlowVersions returns, per validator, the accepted versioned result
// from the highest-priority collection. validator limits the read to one.
func (s *Server) acceptedFlowVersions(chain, validator string) (map[string]servedCommissionFlow, error) {
	sources := s.commissionServingSources(chain)
	out := map[string]servedCommissionFlow{}
	if len(sources) == 0 {
		return out, nil
	}
	versions := make([]string, len(sources))
	for i, src := range sources {
		versions[i] = src.Version
	}
	// Versioned results are only ever inserted, so the row count and newest
	// computed_at identify the whole set; the Hub reuses its decoded set
	// until either changes.
	var stamp flowVersionStamp
	if validator == "" {
		if err := s.db.Table(flowVersionsTable).Select("count(validator) AS n, max(computed_at) AS latest").
			Where("version IN ? AND chain = ?", versions, chain).Scan(&stamp).Error; err != nil {
			return s.flowVersionsUnreadable(out, err)
		}
		s.flowVerMu.Lock()
		c, ok := s.flowVer[chain]
		s.flowVerMu.Unlock()
		if ok && c.stamp.equal(stamp) && c.sources == strings.Join(versions, ",") {
			return c.served, nil
		}
	}
	q := s.db.Table(flowVersionsTable).Select(flowVersionColumns).Where("version IN ? AND chain = ?", versions, chain)
	if validator != "" {
		q = q.Where("validator = ?", validator)
	}
	var rows []flowVersionRow
	if err := q.Scan(&rows).Error; err != nil {
		return s.flowVersionsUnreadable(out, err)
	}
	// The cached set is stamped with what was actually read, so a row
	// inserted between the two reads only causes one more read next time.
	stamp = flowVersionStamp{N: int64(len(rows))}
	for _, r := range rows {
		if r.ComputedAt != nil && (stamp.Latest == nil || r.ComputedAt.After(*stamp.Latest)) {
			t := *r.ComputedAt
			stamp.Latest = &t
		}
	}
	byVersion := map[string][]flowVersionRow{}
	for _, r := range rows {
		byVersion[r.Version] = append(byVersion[r.Version], r)
	}
	for _, src := range sources {
		list := byVersion[src.Version]
		sort.Slice(list, func(i, j int) bool { return list[i].Validator < list[j].Validator })
		for _, r := range list {
			if _, done := out[r.Validator]; done {
				continue
			}
			if served, ok := acceptFlowVersion(src, r, chain); ok {
				out[r.Validator] = served
			}
		}
	}
	if validator == "" {
		s.flowVerMu.Lock()
		if s.flowVer == nil {
			s.flowVer = map[string]flowVersionCache{}
		}
		s.flowVer[chain] = flowVersionCache{stamp: stamp, sources: strings.Join(versions, ","), served: out}
		s.flowVerMu.Unlock()
	}
	return out, nil
}

// flowVersionsUnreadable lets the page keep serving the legacy runs, exactly
// as before versioned results existed, while the database has no versioned
// table (the collectors create it) or the role has not been granted its
// columns. Any other read error fails the request.
func (s *Server) flowVersionsUnreadable(empty map[string]servedCommissionFlow, err error) (map[string]servedCommissionFlow, error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == pgInsufficientPrivilege || pgErr.Code == pgUndefinedTable) {
		if s.flowVerDenied.CompareAndSwap(false, true) {
			log.Printf("commission flows: versioned results not readable, serving legacy runs only: %v", err)
		}
		return empty, nil
	}
	return nil, fmt.Errorf("read versioned commission results: %w", err)
}

const (
	pgInsufficientPrivilege = "42501"
	pgUndefinedTable        = "42P01"
)

type flowVersionStamp struct {
	N      int64
	Latest *time.Time
}

func (a flowVersionStamp) equal(b flowVersionStamp) bool {
	if a.N != b.N || (a.Latest == nil) != (b.Latest == nil) {
		return false
	}
	return a.Latest == nil || a.Latest.Equal(*b.Latest)
}

// flowVersionCache is a decoded accepted set; callers never modify served.
type flowVersionCache struct {
	stamp   flowVersionStamp
	sources string
	served  map[string]servedCommissionFlow
}

const flowVersionsTable = "tax_commission_flow_versions"

// servedCommissionFlows merges accepted versioned results over the legacy
// runs: a validator with an accepted versioned result is served from it,
// any other validator keeps its legacy run.
func (s *Server) servedCommissionFlows(chain, validator string) (map[string]servedCommissionFlow, error) {
	accepted, err := s.acceptedFlowVersions(chain, validator)
	if err != nil {
		return nil, err
	}
	out := make(map[string]servedCommissionFlow, len(accepted))
	for v, sv := range accepted {
		out[v] = sv
	}
	var runs []CommissionFlowRun
	q := s.db.Where("chain = ?", chain)
	if validator != "" {
		q = q.Where("validator = ?", validator)
	}
	if err := q.Find(&runs).Error; err != nil {
		return nil, err
	}
	for _, run := range runs {
		if _, ok := out[run.Validator]; !ok {
			out[run.Validator] = servedCommissionFlow{Run: run}
		}
	}
	return out, nil
}

// servedFlowLines returns the served flow lines for rule: versioned lines
// from the result itself and legacy lines from the legacy flow table.
func (s *Server) servedFlowLines(chain, validator string, served servedCommissionFlow, rule string) ([]CommissionFlow, error) {
	if served.Versioned {
		out := []CommissionFlow{}
		for _, f := range served.Rows {
			if f.Rule == rule {
				out = append(out, f)
			}
		}
		return out, nil
	}
	var flows []CommissionFlow
	err := s.db.Where("chain = ? AND validator = ? AND rule = ?", chain, validator, rule).Find(&flows).Error
	return flows, err
}

// hubFlowLines sums the served lines for rule over the validators in include,
// grouped by month, class, detail and route.
func (s *Server) hubFlowLines(chain, rule string, served map[string]servedCommissionFlow, include map[string]bool) ([]CommissionFlow, error) {
	type key struct {
		month, class, detail string
		via                  bool
	}
	sums := map[key]decimal.Decimal{}
	var legacy []string
	for v := range include {
		sv := served[v]
		if !sv.Versioned {
			legacy = append(legacy, v)
			continue
		}
		for _, f := range sv.Rows {
			if f.Rule != rule {
				continue
			}
			amt, _ := decimal.NewFromString(f.AmountBase)
			k := key{f.Month, f.Class, f.Detail, f.ViaUnbonding}
			sums[k] = sums[k].Add(amt)
		}
	}
	if len(legacy) > 0 {
		sort.Strings(legacy)
		var flows []CommissionFlow
		if err := s.db.Table(CommissionFlow{}.TableName()).
			Select("month, class, detail, via_unbonding, SUM(amount_base)::text AS amount_base").
			Where("chain = ? AND rule = ? AND validator IN ?", chain, rule, legacy).
			Group("month, class, detail, via_unbonding").Scan(&flows).Error; err != nil {
			return nil, err
		}
		for _, f := range flows {
			amt, err := decimal.NewFromString(f.AmountBase)
			if err != nil {
				return nil, fmt.Errorf("legacy flow amount: %w", err)
			}
			k := key{f.Month, f.Class, f.Detail, f.ViaUnbonding}
			sums[k] = sums[k].Add(amt)
		}
	}
	keys := make([]key, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.month != b.month {
			return a.month < b.month
		}
		if a.class != b.class {
			return a.class < b.class
		}
		if a.detail != b.detail {
			return a.detail < b.detail
		}
		return !a.via && b.via
	})
	out := make([]CommissionFlow, 0, len(keys))
	for _, k := range keys {
		out = append(out, CommissionFlow{Chain: chain, Rule: rule, Month: k.month, Class: k.class, Detail: k.detail, ViaUnbonding: k.via, AmountBase: sums[k].String()})
	}
	return out, nil
}
