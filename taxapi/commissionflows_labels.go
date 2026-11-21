package taxapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// Deposit-follow rule: an exchange sweeps what its deposit-style (memo)
// addresses receive into the wallet it pays withdrawals from. The wallet that
// receives at least half of the native coin in a deposit-style address's
// sampled outgoing movements is labelled the same unnamed exchange. Measured
// 2026-11-21 on the 103 memo-deposit addresses with outflows: 62 send at least
// half to one wallet (26 at least 90%), into 42 wallets, one fed by 14 of
// them; the other 41 pay out to many wallets themselves and stay as they are.
const (
	flowFollowHeuristic = "heuristic:deposit-follow/v1"
	flowFollowSample    = 2000
	flowFollowMinShare  = "0.5"
	// flowPayoutReceivers is the supporting wide-payout condition: a
	// withdrawal wallet pays many distinct wallets. It is recorded on every
	// profile but never labels a wallet alone, since reward and airdrop
	// distributors pay crowds too.
	flowPayoutReceivers = 1000
	pgQueryCanceled     = "57014"
)

// ExchangeProfile is what the exchange-like conditions measured on one
// wallet. A wallet is exchange-like when memo-deposit, deposit-sweep or
// deposit-follow holds; wide payout only supports.
type ExchangeProfile struct {
	Address      string
	MemoSenders  int
	SweepExamine int
	SweepPass    int
	FedBy        int
	Receivers    int
	Met          []string
	// Unmeasured names conditions whose query ran past the derivation's
	// statement limit; they count as not met.
	Unmeasured []string `json:",omitempty"`
}

func (p *ExchangeProfile) evaluate() {
	p.Met = nil
	if p.MemoSenders >= flowMemoMinSenders {
		p.Met = append(p.Met, "memo-deposit")
	}
	if sweepExchange(p.SweepExamine, p.SweepPass) {
		p.Met = append(p.Met, "deposit-sweep")
	}
	if p.FedBy > 0 {
		p.Met = append(p.Met, "deposit-follow")
	}
	if p.Receivers >= flowPayoutReceivers {
		p.Met = append(p.Met, "wide-payout")
	}
}

// ExchangeLike reports whether a sufficient condition holds.
func (p ExchangeProfile) ExchangeLike() bool {
	for _, m := range p.Met {
		if m != "wide-payout" {
			return true
		}
	}
	return false
}

func (p ExchangeProfile) String() string {
	return fmt.Sprintf("conditions met: %s (memo-deposit senders %d of %d needed; deposit-sweep %d of %d sampled senders pass through; fed by %d deposit-style address(es); pays %d distinct wallets, %d+ supports)",
		strings.Join(p.Met, ", "), p.MemoSenders, flowMemoMinSenders, p.SweepPass, p.SweepExamine, p.FedBy, p.Receivers, flowPayoutReceivers)
}

// exchangeProfile measures every condition on addr over [from, asOf). A
// measurement cancelled by the statement limit is recorded as unmeasured.
func (s *Server) exchangeProfile(addr string, fedBy int, from, asOf time.Time) (ExchangeProfile, error) {
	p := ExchangeProfile{Address: addr, FedBy: fedBy}
	measure := func(name string, err error) error {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgQueryCanceled {
			p.Unmeasured = append(p.Unmeasured, name)
			return nil
		}
		return err
	}
	var err error
	p.MemoSenders, err = s.memoDepositSenders(addr)
	if err = measure("memo-deposit", err); err != nil {
		return p, err
	}
	p.SweepExamine, p.SweepPass, err = s.sweepDepositSenders(addr, from, asOf)
	if err = measure("deposit-sweep", err); err != nil {
		return p, err
	}
	p.Receivers, err = s.fanoutReceivers(addr, from, asOf, flowPayoutReceivers*100)
	if err = measure("wide-payout", err); err != nil {
		return p, err
	}
	p.evaluate()
	return p, nil
}

// CommissionLabelFile is an immutable private list of address labels derived
// outside a trace run (deposit following, chain registries, published
// reserve lists). A run that uses one names its digest in the version, so
// its rows never mix with a run that used another list.
type CommissionLabelFile struct {
	Chain      string
	Rule       string
	From       time.Time
	AsOf       time.Time
	Labels     []AddressLabel
	Unfollowed []CommissionLabelUnfollowed `json:",omitempty"`
	// Profiles holds the measured conditions for every candidate: the
	// stored memo-deposit addresses and the wallets they sweep into.
	Profiles []ExchangeProfile `json:",omitempty"`
	digest   string
}

// CommissionLabelUnfollowed records a deposit-style address the rule left
// unlabelled downstream, with what was measured.
type CommissionLabelUnfollowed struct {
	Deposit  string
	Reason   string
	Transfer int
}

// labelFileKinds are the kinds a label file may carry: each has its own
// class in labelStop. A named exchange must come from the exchange's own
// published list.
var labelFileKinds = map[string]bool{
	labelExchange: true, labelExchangeLike: true, labelModule: true, labelIBCEscrow: true, labelValidatorOperator: true,
	labelInterchainAccount: true, labelGrantRecipient: true, labelBridgeEscrow: true, labelBridgeContract: true, labelDexPool: true, labelContract: true,
}

func (f *CommissionLabelFile) validate(chain string) error {
	if f.Chain != chain || f.Rule == "" || f.From.IsZero() || f.AsOf.IsZero() {
		return fmt.Errorf("label file must name its chain, rule and window")
	}
	seen := map[string]bool{}
	for _, l := range f.Labels {
		if l.Chain != chain || l.Address == "" || seen[l.Address] {
			return fmt.Errorf("label file has a label for another chain, an empty or a repeated address")
		}
		seen[l.Address] = true
		if !labelFileKinds[l.Kind] || l.Name == "" || l.Evidence == "" || l.CreatedAt.IsZero() ||
			!(strings.HasPrefix(l.Source, "heuristic:") || strings.HasPrefix(l.Source, "derived:") || strings.HasPrefix(l.Source, "published:")) {
			return fmt.Errorf("label file label needs a known kind, name, evidence, date and a heuristic, derived or published source")
		}
		if l.Kind == labelExchange && !strings.HasPrefix(l.Source, "published:") {
			return fmt.Errorf("only an exchange's own published list names an exchange")
		}
	}
	return nil
}

// LoadCommissionLabelFile reads a label file whose bytes match expected.
func LoadCommissionLabelFile(path, expected, chain string) (*CommissionLabelFile, error) {
	original, err := os.Lstat(path)
	if err != nil || !original.Mode().IsRegular() {
		return nil, fmt.Errorf("label file must be a regular non-symlink file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<20 || !correctionHash(expected) || privateSHA(b) != expected {
		return nil, fmt.Errorf("label file digest mismatch")
	}
	var lf CommissionLabelFile
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&lf) != nil {
		return nil, fmt.Errorf("label file decode failed")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("label file trailing data")
	}
	if err := lf.validate(chain); err != nil {
		return nil, err
	}
	lf.digest = expected
	return &lf, nil
}

// DeriveDepositFollowLabels applies the deposit-follow rule to every stored
// memo-deposit label over [from, asOf). It only reads.
func (s *Server) DeriveDepositFollowLabels(chain string, from, asOf time.Time) (*CommissionLabelFile, error) {
	var labels []AddressLabel
	if err := s.db.Where("chain = ?", chain).Find(&labels).Error; err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var deposits []string
	for _, l := range labels {
		known[l.Address] = true
		if l.Kind == labelExchangeLike && l.Source == flowMemoHeuristic {
			deposits = append(deposits, l.Address)
		}
	}
	sort.Strings(deposits)
	type feed struct {
		deposit       string
		amount, total decimal.Decimal
		transfers     int
	}
	fed := map[string][]feed{}
	out := &CommissionLabelFile{Chain: chain, Rule: flowFollowHeuristic, From: from, AsOf: asOf}
	for _, dep := range deposits {
		var rows []struct {
			ToAddr   string
			Category string
			Amount   decimal.Decimal
			N        int
		}
		if err := s.db.Raw(`
SELECT to_addr, category, sum(amount::numeric) AS amount, count(*) AS n FROM (
	SELECT to_addr, category, amount FROM taxable_events
	WHERE from_addr = ? AND denom = ? AND timestamp >= ? AND timestamp < ?
	ORDER BY timestamp DESC LIMIT ?) o
GROUP BY to_addr, category`, dep, s.native.Denom, from, asOf, flowFollowSample).Scan(&rows).Error; err != nil {
			return nil, err
		}
		total, n := decimal.Zero, 0
		var top struct {
			to     string
			amount decimal.Decimal
			n      int
		}
		for _, r := range rows {
			total, n = total.Add(r.Amount), n+r.N
			if r.Category == apiTransfer && r.ToAddr != "" && r.ToAddr != dep && (r.Amount.GreaterThan(top.amount) || (r.Amount.Equal(top.amount) && r.ToAddr < top.to)) {
				top.to, top.amount, top.n = r.ToAddr, r.Amount, r.N
			}
		}
		switch {
		case n == 0:
			out.Unfollowed = append(out.Unfollowed, CommissionLabelUnfollowed{Deposit: dep, Reason: "no outgoing movements in the window"})
		case !total.IsPositive() || top.amount.LessThan(total.Mul(decimal.RequireFromString(flowFollowMinShare))):
			out.Unfollowed = append(out.Unfollowed, CommissionLabelUnfollowed{Deposit: dep, Reason: "pays out to many wallets itself; no wallet receives half", Transfer: n})
		case known[top.to] || contractAddress(top.to):
			out.Unfollowed = append(out.Unfollowed, CommissionLabelUnfollowed{Deposit: dep, Reason: "sweeps into an already labelled or contract address", Transfer: n})
		default:
			fed[top.to] = append(fed[top.to], feed{deposit: dep, amount: top.amount, total: total, transfers: top.n})
		}
	}
	for _, dep := range deposits {
		p, err := s.exchangeProfile(dep, 0, from, asOf)
		if err != nil {
			return nil, err
		}
		out.Profiles = append(out.Profiles, p)
	}
	now := nowUTC()
	for to, feeds := range fed {
		p, err := s.exchangeProfile(to, len(feeds), from, asOf)
		if err != nil {
			return nil, err
		}
		out.Profiles = append(out.Profiles, p)
		var parts []string
		for _, f := range feeds {
			parts = append(parts, fmt.Sprintf("%s of %s from %s in %d transfers", f.amount, f.total, f.deposit, f.transfers))
		}
		out.Labels = append(out.Labels, AddressLabel{
			Chain: chain, Address: to, Name: "withdrawal wallet of an unnamed exchange", Kind: labelExchangeLike, Source: flowFollowHeuristic, CreatedAt: now,
			Evidence: fmt.Sprintf("inferred; %s; receives at least half of the %s sent in the latest %d outgoing movements of %d deposit-style address(es) from %s to %s: %s",
				p, s.native.Denom, flowFollowSample, len(feeds), from.Format(apiDateLayout), asOf.Format(apiDateLayout), strings.Join(parts, "; ")),
		})
	}
	sort.Slice(out.Labels, func(i, j int) bool { return out.Labels[i].Address < out.Labels[j].Address })
	sort.Slice(out.Profiles, func(i, j int) bool { return out.Profiles[i].Address < out.Profiles[j].Address })
	return out, nil
}

// WriteCommissionLabelFile writes lf and returns the digest a run must name.
func WriteCommissionLabelFile(path string, lf *CommissionLabelFile) (string, error) {
	b, err := json.MarshalIndent(lf, "", " ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return privateSHA(b), nil
}
