package taxapi

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExchangeProfileConditions(t *testing.T) {
	p := ExchangeProfile{Receivers: 5000}
	p.evaluate()
	require.Equal(t, []string{"wide-payout"}, p.Met)
	require.False(t, p.ExchangeLike(), "paying a crowd alone is a distributor, not an exchange")
	p = ExchangeProfile{MemoSenders: flowMemoMinSenders}
	p.evaluate()
	require.True(t, p.ExchangeLike())
	p = ExchangeProfile{SweepExamine: 200, SweepPass: 120, FedBy: 2, Receivers: 999}
	p.evaluate()
	require.Equal(t, []string{"deposit-sweep", "deposit-follow"}, p.Met)
	require.Contains(t, p.String(), "fed by 2 deposit-style address(es)")
}

func TestCommissionLabelFileLoadAndNaming(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	lf := &CommissionLabelFile{Chain: apiMainnet, Rule: flowFollowHeuristic, From: flowPGWindowStart, AsOf: at, Labels: []AddressLabel{
		{Chain: apiMainnet, Address: "cosmos1withdrawal", Name: "withdrawal wallet of an unnamed exchange", Kind: labelExchangeLike, Source: flowFollowHeuristic, Evidence: "inferred; conditions met: deposit-follow", CreatedAt: at},
	}}
	path := filepath.Join(dir, "labels.json")
	digest, err := WriteCommissionLabelFile(path, lf)
	require.NoError(t, err)
	_, err = LoadCommissionLabelFile(path, strings.Repeat("0", 64), apiMainnet)
	require.Error(t, err, "digest must match")
	_, err = LoadCommissionLabelFile(path, digest, "testnet")
	require.Error(t, err, "chain must match")
	loaded, err := LoadCommissionLabelFile(path, digest, apiMainnet)
	require.NoError(t, err)
	require.Len(t, loaded.Labels, 1)

	bad := *lf
	bad.Labels = []AddressLabel{lf.Labels[0]}
	bad.Labels[0].Kind = labelExchange
	badPath := filepath.Join(dir, "bad.json")
	badDigest, err := WriteCommissionLabelFile(badPath, &bad)
	require.NoError(t, err)
	_, err = LoadCommissionLabelFile(badPath, badDigest, apiMainnet)
	require.Error(t, err, "a named exchange needs a published source")
	bad.Labels[0].Source = "published:por-test/v1"
	badDigest, err = WriteCommissionLabelFile(badPath, &bad)
	require.NoError(t, err)
	_, err = LoadCommissionLabelFile(badPath, badDigest, apiMainnet)
	require.NoError(t, err, "an exchange's own published list names it")
	bad.Labels[0].Kind = "mixer"
	badDigest, err = WriteCommissionLabelFile(badPath, &bad)
	require.NoError(t, err)
	_, err = LoadCommissionLabelFile(badPath, badDigest, apiMainnet)
	require.Error(t, err, "an unknown kind is refused")
	require.NoError(t, os.Symlink(path, filepath.Join(dir, "link.json")))
	_, err = LoadCommissionLabelFile(filepath.Join(dir, "link.json"), digest, apiMainnet)
	require.Error(t, err)

	profile := &CommissionPrivateProfile{digest: strings.Repeat("a", 64)}
	o := CommissionFlowBuildOptions{Version: "ops847-profile" + strings.Repeat("a", 12), Build: "b", AsOf: at, PrivateProfile: profile, Labels: loaded}
	o.PrivateProfile.AsOf = at
	require.Error(t, o.validate(), "the version must name the label file")
	o.Version += "-labels" + digest[:12]
	require.NoError(t, o.validate())
	o.Labels = nil
	require.Error(t, o.validate(), "a label-named version needs its file")
}

// Deposit-style addresses that sweep into one wallet label it as the same
// unnamed exchange, with every condition measured; one that pays out widely
// or sweeps into an already labelled address is recorded unfollowed.
func TestDeriveDepositFollowLabelsPostgres(t *testing.T) {
	s := flowPGServer(t)
	const w = "cosmos1withdrawal"
	now := nowUTC()
	for _, d := range []string{"cosmos1depa", "cosmos1depb", "cosmos1depc", "cosmos1depd", "cosmos1depe"} {
		require.NoError(t, s.db.Create(&AddressLabel{Chain: apiMainnet, Address: d, Name: "deposit-style address", Kind: labelExchangeLike, Source: flowMemoHeuristic, Evidence: "test", CreatedAt: now}).Error)
	}
	require.NoError(t, s.db.Create(&AddressLabel{Chain: apiMainnet, Address: "cosmos1escrow", Name: "transfer/channel-0", Kind: labelIBCEscrow, Source: "derived:ics20-escrow", Evidence: "test", CreatedAt: now}).Error)
	for _, sql := range []string{
		// a: 90% to the withdrawal wallet; b: all of it; c: five payees.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) VALUES ('transfer','900','uatom','cosmos1depa','" + w + "','2025-02-01T00:00:00Z'),('transfer','100','uatom','cosmos1depa','cosmos1someone','2025-02-02T00:00:00Z')",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer','50','uatom','cosmos1depb','" + w + "','2025-02-01T00:00:00Z' FROM generate_series(1,4)",
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer','10','uatom','cosmos1depc','cosmos1payee'||g,'2025-02-01T00:00:00Z' FROM generate_series(1,5) g",
		// d sweeps into a labelled escrow; e moved only outside the window.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) VALUES ('transfer','10','uatom','cosmos1depd','cosmos1escrow','2025-02-01T00:00:00Z'),('transfer','10','uatom','cosmos1depe','" + w + "','2025-06-01T00:00:00Z')",
		// The withdrawal wallet pays three customers.
		"INSERT INTO taxable_events (category, amount, denom, from_addr, to_addr, timestamp) SELECT 'transfer','5','uatom','" + w + "','cosmos1customer'||g,'2025-02-03T00:00:00Z' FROM generate_series(1,3) g",
	} {
		require.NoError(t, s.db.Exec(sql).Error, sql)
	}
	lf, err := s.DeriveDepositFollowLabels(apiMainnet, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Len(t, lf.Labels, 1)
	lab := lf.Labels[0]
	require.Equal(t, w, lab.Address)
	require.Equal(t, labelExchangeLike, lab.Kind)
	require.Equal(t, flowFollowHeuristic, lab.Source)
	require.Contains(t, lab.Evidence, "inferred; conditions met: deposit-follow")
	require.Contains(t, lab.Evidence, "fed by 2 deposit-style address(es)")
	require.Contains(t, lab.Evidence, "900 of 1000 from cosmos1depa in 1 transfers")
	require.Contains(t, lab.Evidence, "200 of 200 from cosmos1depb in 4 transfers")
	reasons := map[string]string{}
	for _, u := range lf.Unfollowed {
		reasons[u.Deposit] = u.Reason
	}
	require.Equal(t, map[string]string{
		"cosmos1depc": "pays out to many wallets itself; no wallet receives half",
		"cosmos1depd": "sweeps into an already labelled or contract address",
		"cosmos1depe": "no outgoing movements in the window",
	}, reasons)
	require.Len(t, lf.Profiles, 6, "five deposit-style addresses and the wallet they feed")
	for _, p := range lf.Profiles {
		if p.Address == w {
			require.Equal(t, 3, p.Receivers)
			require.Equal(t, []string{"deposit-follow"}, p.Met)
		}
	}
	require.NoError(t, lf.validate(apiMainnet))

	// A run with the file stops at the withdrawal wallet as exchange-like.
	l := &dbFlowLoader{s: s, chain: apiMainnet, labels: map[string]AddressLabel{w: lab}, mu: &sync.Mutex{}, chans: map[string]string{}, private: true}
	stop, err := l.stop(w, flowPGWindowStart, flowPGWindowEnd)
	require.NoError(t, err)
	require.Equal(t, flowStop{Class: flowExchangeLike, Detail: flowFollowHeuristic}, stop)
	var stored int64
	require.NoError(t, s.db.Model(&AddressLabel{}).Where("source = ?", flowFollowHeuristic).Count(&stored).Error)
	require.Zero(t, stored, "deriving stores nothing")
}

// Every OPS-849 kind stops in its own class; only a published exchange is an
// exchange, and an unknown kind never falls through to one.
func TestLabelKindsStopClasses(t *testing.T) {
	for kind, class := range map[string]string{
		labelInterchainAccount: flowInterchainAccount,
		labelGrantRecipient:    flowGrantRecipient,
		labelBridgeEscrow:      flowBridge,
		labelBridgeContract:    flowBridge,
		labelDexPool:           flowDexPool,
		labelContract:          flowContract,
		labelExchange:          flowExchange,
		labelIBCEscrow:         flowIBCOut,
	} {
		require.Equal(t, class, labelStop(AddressLabel{Kind: kind, Name: "n"}).Class, kind)
		require.True(t, labelKindsKnown[kind], kind)
		require.NotEmpty(t, FlowClassNotes[class], class)
	}
	require.Panics(t, func() { labelStop(AddressLabel{Kind: "mixer"}) })
}
