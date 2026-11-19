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

// The giant reruns stop tracing at a wallet that sent ATOM to more than
// 50,000 receivers. They share the corrected profile, window and version
// prefix but run their own build. Most entries serve a validator whose run
// never reached such a wallet, so its result is the one the corrected build
// computes. The rest reached one, never finished on the corrected build, and
// booked under 0.1% of their commission to wide_distributor, which bounds
// what the stop can change.
const flowGiantBuild = "sha256:324924bc987eeaa3d0475a9c0bda70cddf718027c421c890ca90663389eeba46"

func flowGiantRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowGiantBuild
	return r
}

// The withdraw-address reruns fix reward rows that credited the delegator
// when the chain paid a different withdraw address. They keep the corrected
// profile, window and version prefix, run their own build, and each serves
// only the validator whose rerun passed acceptance against its served row.
const flowWithdrawFixBuild = "sha256:ae39819c1af339987de665826c35edb317e6ad11b5390d3d7e9a5476228c8e80"

func flowWithdrawFixRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowWithdrawFixBuild
	return r
}

// defaultCommissionServingSources is in priority order: registered retries
// first, then the corrected shards, then the two source collections the
// shards adopt from. A validator is served from the first collection that
// holds an accepted result for it.
var defaultCommissionServingSources = map[string][]commissionServingSource{
	apiMainnet: {
		flowWithdrawFixRoot("accel-withdraw-fix-pilot-20261118-fanout-v1", "c367934baa77ab0ce85a84cbd4ec158dfcfdac3f0df7e6efdbf5d02eaff5938b"),
		flowWithdrawFixRoot("accel-withdraw-fix-52836aab8ac6-20261118-fanout-v1", "52836aab8ac6250910ea4e77539c7ef6ef8fa98050b057271b80919434a13e42"),
		flowWithdrawFixRoot("accel-withdraw-fix-0a11a4c00904-20261118-fanout-v1", "0a11a4c0090436746a12555ec8f07a503c467e849bccf9ef550a8e2985f2b5ab"),
		flowWithdrawFixRoot("accel-withdraw-fix-d2d7b3c161a2-20261118-fanout-v1", "d2d7b3c161a280621a0b05189e3746f1d785eab94cc4de6096a76a621c80d2b5"),
		flowWithdrawFixRoot("accel-withdraw-fix-001308de5051-20261118-fanout-v1", "001308de505138aa91ea460b148382f1dd7b6528a4a307e6333dff716f08a7ae"),
		flowWithdrawFixRoot("accel-withdraw-fix-ab12977c1a21-20261118-fanout-v1", "ab12977c1a2135f7aeea69534171ee3c2bec9a03f3650cbf5dff76caefb19345"),
		flowWithdrawFixRoot("accel-withdraw-fix-a5b85d99e09a-20261118-fanout-v1", "a5b85d99e09a1b9844bbb4209119b7b1d730b83b37c8e45bae11e21c09428965"),
		flowWithdrawFixRoot("accel-withdraw-fix-06d2a6fb6513-20261118-fanout-v1", "06d2a6fb65137dc2b6730a227548da0ab3d738cc6bf2a561c68ed9154c293c1b"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "5e6e5429e48f651ba98b03e3791deda86e390d13112e503e902939d08d86aa10"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "e5ede2045f7c06ec1da4432418d34d7f4f4a2a429d88531a60e0f7eb23a8cd64"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "fcea1968b7af091e011302f4924bd95858b19dd48001e6cf02fd700e691f5bcb"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "274a1c06a584325f3d85da4a602930b04d10455e39edbd665ccffad5e1ddf7d9"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "86c5c4e665fef7b0671134a194322556b90ff8e12ee4a9f5e80839c7a300d1dc"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "e2f8b175c38b90fad6460e863546e042d20db6b5814f346936023922c2912c55"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "91839f8b6913340a41e7915e84d50818dcadff35b8a7a04e3b367a4cb098fb3b"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "2877ad667da5260e6188746799121924cb3fd1e13043450dad6192ec7387803a"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "55b998caa2945907f1b1177130043fd61b382dc3de466e51aacb22f058164509"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "8af885ef7203f0e9e32249b1580b75c12374417434c72ca643d534a8a64f7570"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "0c019c02d744f8202ad5771e5c22e143caaeb432a0cc81c62d81a18ae78a93de"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "bb7050112168b67a4ae2a62d5db9f0f27bfa83c0772458919caeedef5d0ea1d4"),
		flowGiantRoot("accel-four-s01-20261118-fanout-v2", "317097d52dd71d08f60d71a3622af4d4a442ebd012cd4e99d98367746de5b17d"),
		flowGiantRoot("accel-four-s01b-20261118-fanout-v2", "801dfe33c567eff0a1738d23a29982dabf76b1c85452210cb837315c7e818745"),
		flowGiantRoot("accel-four-s01-20261118-fanout-v2", "48b61e4b3eeb060cdbcea7dab31dff25bf04be77c344da0d32c0d6e68f8f70ba"),
		flowGiantRoot("accel-four-s01b-20261118-fanout-v2", "066df80725a01e1c27270b034e66486b6cd46d04d45e8e49b357c1a710db29b4"),
		flowGiantRoot("accel-four-s01b-20261118-fanout-v2", "31c1763c0d2bfaa85a33b9af53f0d7b9b8249cf2d225145ff2e3e23da2bc51be"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "4e09d1dfc7e6cdb78129a723dbc0d459052d8adbcc6e704a492520f5c05211f9"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "9cd3bff2c9449e24004bfe0fa0d08b8466fb60c4d7666e33a8826bf4e92b313e"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "11ad1e6695406f2ec282e61c3d2a68a278336c6d6c70a999f31aeb78b5315354"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "607606f87da17ce8a8581cda77daff29c9c325a011b63fafc4a30fbf29efaff5"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "a21b65a9b88ec88fd0ad5015fe2907e2d89971bad4fe92d3808889b807198e1a"),
		flowGiantRoot("accel-four-s07-r300-20261118-fanout-v2", "320bcb826109bdff2fcab865b3971af8023c5d8b2ba24f02961a4faca99425aa"),
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
