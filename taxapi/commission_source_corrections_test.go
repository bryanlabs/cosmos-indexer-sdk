package taxapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func correctionBool(v bool) *bool { return &v }
func correctionTestManifest(t *testing.T) *CommissionSourceCorrections {
	t.Helper()
	from := time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	p := &CommissionSourceCorrections{Schema: commissionCorrectionSchema, Chain: "cosmoshub-4", From: from, AsOf: end, BaseProfileSHA256: strings.Repeat("a", 64), FiveAuthoritySHA256: correctionFiveAuthority, SevenAuthoritySHA256: correctionSevenAuthority, SixAuthoritySHA256: correctionSixAuthority, NormalizerSHA256: strings.Repeat("b", 64)}
	for i := 0; i < 6; i++ {
		wallet := "payer"
		if i == 3 {
			wallet = "reward-a"
		}
		if i == 4 {
			wallet = "reward-b"
		}
		p.Windows = append(p.Windows, CommissionCorrectionWindow{ID: fmt.Sprintf("window-%d", i), Wallet: wallet, Start: from, End: end, SourceInputSHA256: privateSHA([]byte(fmt.Sprint(i)))})
	}
	at := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 786; i++ {
		authority := correctionFiveAuthority
		window := "window-0"
		if i >= 779 {
			authority = correctionSevenAuthority
			window = "window-5"
		}
		members := []string{window}
		if i == 0 {
			members = append(members, "window-1", "window-2")
		}
		p.Fees = append(p.Fees, CommissionNativeFeeCorrection{CommissionCorrectionIdentity: CommissionCorrectionIdentity{ID: privateSHA([]byte(fmt.Sprintf("fee-%d", i))), TxHash: fmt.Sprintf("%064X", i+1), Height: int64(1000 + i), Timestamp: at.Add(time.Duration(i) * time.Second), ProofSHA256: []string{privateSHA([]byte(fmt.Sprintf("proof-%d", i)))}, Windows: members}, ChargedActor: "payer", IndexedActor: "constituent", SourceNominal: map[string]string{apiUatom: "10"}, Gross: map[string]string{apiUatom: "10"}, Refund: map[string]string{apiUatom: "3"}, Net: map[string]string{apiUatom: "7"}, Authorities: []string{authority}, ChargedActorEventsSHA256: correctionEventsSHA(nil, "", 0), SourceBANKFeeDebitOverlap: correctionBool(false)})
	}
	for i := 0; i < 6; i++ {
		recipient := "reward-a"
		window := "window-3"
		tx := 10000
		event := i
		if i >= 4 {
			recipient = "reward-b"
			window = "window-4"
			tx++
			event -= 4
		}
		p.Rewards = append(p.Rewards, CommissionNativeRewardCorrection{CommissionCorrectionIdentity: CommissionCorrectionIdentity{ID: privateSHA([]byte(fmt.Sprintf("reward-%d", i))), TxHash: fmt.Sprintf("%064X", tx), Height: int64(tx), Timestamp: at.AddDate(0, 1, tx-10000), ProofSHA256: []string{privateSHA([]byte(fmt.Sprintf("reward-proof-%d", i)))}, Windows: []string{window}}, Recipient: recipient, Denom: apiUatom, Amount: "2", NativeMessageIndex: 4, NativeEventIndex: event, MessageType: "/cosmos.staking.v1beta1.MsgWithdrawAllTokenizeShareRecordReward", OriginalMessageEventsSHA256: correctionEventsSHA(nil, "", 0), SourceBANKCreditOverlap: correctionBool(false)})
	}
	require.NoError(t, p.validate())
	p.digest = strings.Repeat("c", 64)
	return p
}
func correctionTestSQL(t *testing.T, p *CommissionSourceCorrections) (*Server, sqlmock.Sqlmock) {
	t.Helper()
	raw, m, e := sqlmock.New()
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); raw.Close() })
	db, e := gorm.Open(postgres.New(postgres.Config{Conn: raw}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
	require.NoError(t, e)
	return &Server{db: db, readOnlyLedger: true, readSnapshotStarted: time.Now(), privateProjectionSnapshot: "fixture-only-snapshot", commissionSourceCorrections: p, sourceCorrectionReceipts: newCorrectionJournal()}, m
}
func expectCorrectionTx(m sqlmock.Sqlmock, c CommissionCorrectionIdentity) {
	m.ExpectQuery("SELECT x.id,x.hash").WillReturnRows(sqlmock.NewRows([]string{"id", "hash", "height", "timestamp", "code"}).AddRow(42, c.TxHash, c.Height, c.Timestamp.Truncate(time.Microsecond), c.Code))
}
func expectCorrectionFee(m sqlmock.Sqlmock, c CommissionNativeFeeCorrection, actor string, grants int) {
	expectCorrectionTx(m, c.CommissionCorrectionIdentity)
	rows := sqlmock.NewRows([]string{"actor", "denom", "amount"})
	if actor != "" {
		rows.AddRow(actor, apiUatom, "10")
	}
	m.ExpectQuery("SELECT a.address AS actor").WillReturnRows(rows)
	m.ExpectQuery("SELECT count.*tax_fee_grants").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(grants))
	if grants == 0 {
		m.ExpectQuery("SELECT m.id FROM messages").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(43))
	}
}

func TestCommissionSourceCorrectionManifestBoundedNativeUnion(t *testing.T) {
	p := correctionTestManifest(t)
	raw, e := json.Marshal(p)
	require.NoError(t, e)
	path := filepath.Join(t.TempDir(), "profile.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	got, e := LoadCommissionSourceCorrections(path, privateSHA(raw))
	require.NoError(t, e)
	require.Len(t, got.Fees, 786)
	require.Len(t, got.Rewards, 6)
	require.Len(t, got.feesByWallet["constituent"], 786)
	require.Len(t, got.Windows, 6)
	_, e = LoadCommissionSourceCorrections(path, strings.Repeat("f", 64))
	require.Error(t, e)
	symlink := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(path, symlink))
	_, e = LoadCommissionSourceCorrections(symlink, privateSHA(raw))
	require.Error(t, e)
}
func TestCommissionSourceCorrectionManifestRejectsUnprovedFacts(t *testing.T) {
	mutations := map[string]func(*CommissionSourceCorrections){"authority": func(p *CommissionSourceCorrections) { p.FiveAuthoritySHA256 = strings.Repeat("f", 64) }, "chain": func(p *CommissionSourceCorrections) { p.Chain = "other" }, "missing_window": func(p *CommissionSourceCorrections) { p.Windows = p.Windows[:5] }, "duplicate_window": func(p *CommissionSourceCorrections) { p.Windows[1] = p.Windows[0] }, "missing_membership": func(p *CommissionSourceCorrections) { p.Fees[0].Windows = []string{"missing"} }, "outside_native_date": func(p *CommissionSourceCorrections) { p.Fees[0].Timestamp = p.AsOf }, "wrong_membership_actor": func(p *CommissionSourceCorrections) { p.Fees[0].ChargedActor = "other" }, "negative_fee": func(p *CommissionSourceCorrections) { p.Fees[0].Net[apiUatom] = "-1" }, "noncanonical_amount": func(p *CommissionSourceCorrections) { p.Fees[0].Net[apiUatom] = "07" }, "refund_mismatch": func(p *CommissionSourceCorrections) { p.Fees[0].Refund[apiUatom] = "4" }, "unknown_refund": func(p *CommissionSourceCorrections) { p.Fees[0].Refund = nil }, "overlapping_BANK_fee": func(p *CommissionSourceCorrections) { p.Fees[0].SourceBANKFeeDebitOverlap = correctionBool(true) }, "unknown_BANK_fee": func(p *CommissionSourceCorrections) { p.Fees[0].SourceBANKFeeDebitOverlap = nil }, "overlapping_BANK_reward": func(p *CommissionSourceCorrections) { p.Rewards[0].SourceBANKCreditOverlap = correctionBool(true) }, "unknown_BANK_reward": func(p *CommissionSourceCorrections) { p.Rewards[0].SourceBANKCreditOverlap = nil }, "duplicate_tx": func(p *CommissionSourceCorrections) { p.Fees[1].TxHash = p.Fees[0].TxHash }, "duplicate_reward": func(p *CommissionSourceCorrections) { p.Rewards[1].NativeEventIndex = p.Rewards[0].NativeEventIndex }, "invented_native_event_index": func(p *CommissionSourceCorrections) { p.Rewards[0].NativeEventIndex = -1 }, "invalid_native_hash": func(p *CommissionSourceCorrections) { p.Fees[0].TxHash = strings.Repeat("x", 64) }, "source_footprint_missing": func(p *CommissionSourceCorrections) { p.Fees[0].ChargedActorEventsSHA256 = "" }, "missing_proof": func(p *CommissionSourceCorrections) { p.Fees[0].ProofSHA256 = nil }, "authority_scope_loss": func(p *CommissionSourceCorrections) { p.Fees[785].Authorities = []string{correctionFiveAuthority} }}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) { p := correctionTestManifest(t); change(p); require.Error(t, p.validate()) })
	}
}
func TestCommissionSourceCorrectionProfileDecodeClosed(t *testing.T) {
	p := correctionTestManifest(t)
	b, e := json.Marshal(p)
	require.NoError(t, e)
	for _, raw := range [][]byte{append(append([]byte(nil), b...), []byte(" {}")...), append([]byte(`{"unexpected":true,`), b[1:]...)} {
		path := filepath.Join(t.TempDir(), "profile")
		require.NoError(t, os.WriteFile(path, raw, 0600))
		_, e := LoadCommissionSourceCorrections(path, privateSHA(raw))
		require.Error(t, e)
	}
}

func TestCommissionSourceCorrectionFeesConsumeNativeNetOnly(t *testing.T) {
	for _, actor := range []string{"payer", "constituent"} {
		t.Run(actor, func(t *testing.T) {
			p := correctionTestManifest(t)
			s, m := correctionTestSQL(t, p)
			c := p.Fees[0]
			expectCorrectionFee(m, c, "constituent", 0)
			rows := walletRows{Fees: []feeRec{{Hash: "unrelated", Amount: "4", Denom: apiUatom}}, Memos: map[string]string{"other": "preserved"}}
			original, e := json.Marshal(rows)
			require.NoError(t, e)
			out, e := s.applyCommissionSourceCorrections(apiMainnet, actor, c.Timestamp, c.Timestamp.Add(time.Second), rows)
			require.NoError(t, e)
			require.Equal(t, rows.Events, out.Events)
			require.Equal(t, rows.Memos, out.Memos)
			if actor == "payer" {
				require.Len(t, out.Fees, 2)
				require.Equal(t, "7", out.Fees[1].Amount)
			} else {
				require.Len(t, out.Fees, 1)
			}
			after, e := json.Marshal(rows)
			require.NoError(t, e)
			require.Equal(t, string(original), string(after))
			r := s.sourceCorrectionReceipts.receipts()
			require.Len(t, r, 1)
			require.Equal(t, "bounded_native_effects_consumed", r[0].Status)
			require.Len(t, r[0].Effects, 1)
			require.Equal(t, c.ID, r[0].Effects[0].EffectID)
			require.Equal(t, c.ProofSHA256, r[0].Effects[0].ProofSHA256)
		})
	}
}
func TestCommissionSourceCorrectionNoIndexedFeeStillUsesActualNative(t *testing.T) {
	p := correctionTestManifest(t)
	s, m := correctionTestSQL(t, p)
	c := p.Fees[0]
	expectCorrectionFee(m, c, "", 0)
	out, e := s.applyCommissionSourceCorrections(apiMainnet, "payer", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{})
	require.NoError(t, e)
	require.Len(t, out.Fees, 1)
	require.Equal(t, "7", out.Fees[0].Amount)
}
func TestCommissionSourceCorrectionBankSourceChangeFails(t *testing.T) {
	p := correctionTestManifest(t)
	s, m := correctionTestSQL(t, p)
	c := p.Fees[0]
	expectCorrectionFee(m, c, "constituent", 0)
	_, e := s.applyCommissionSourceCorrections(apiMainnet, "payer", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{Events: []tax.TaxableEvent{{TxHash: c.TxHash, Category: apiTransfer, Amount: "7", Denom: apiUatom, FromAddr: "payer", ToAddr: "fee-module"}}})
	require.ErrorContains(t, e, "footprint changed")
	require.Empty(t, s.sourceCorrectionReceipts.receipts())
}
func TestCommissionSourceCorrectionUnsupportedGrantFails(t *testing.T) {
	p := correctionTestManifest(t)
	s, m := correctionTestSQL(t, p)
	c := p.Fees[0]
	expectCorrectionFee(m, c, "constituent", 1)
	_, e := s.applyCommissionSourceCorrections(apiMainnet, "payer", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{})
	require.ErrorContains(t, e, "grant")
	require.Empty(t, s.sourceCorrectionReceipts.receipts())
}
func TestCommissionSourceCorrectionNoDoubleApply(t *testing.T) {
	p := correctionTestManifest(t)
	s, m := correctionTestSQL(t, p)
	c := p.Fees[0]
	expectCorrectionFee(m, c, "constituent", 0)
	_, e := s.applyCommissionSourceCorrections(apiMainnet, "payer", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{Fees: []feeRec{{Hash: c.TxHash, Amount: "7", Denom: apiUatom}}})
	require.ErrorContains(t, e, "not deferred")
	require.Empty(t, s.sourceCorrectionReceipts.receipts())
}
func TestCommissionSourceCorrectionNativeTransactionIdentity(t *testing.T) {
	for _, field := range []string{"id", "hash", "height", "timestamp", "code"} {
		t.Run(field, func(t *testing.T) {
			p := correctionTestManifest(t)
			s, m := correctionTestSQL(t, p)
			c := p.Fees[0]
			id := 42
			hash := c.TxHash
			height := c.Height
			at := c.Timestamp
			code := c.Code
			switch field {
			case "id":
				id = 0
			case "hash":
				hash = "other"
			case "height":
				height++
			case "timestamp":
				at = at.Add(time.Microsecond)
			case "code":
				code++
			}
			m.ExpectQuery("SELECT x.id,x.hash").WillReturnRows(sqlmock.NewRows([]string{"id", "hash", "height", "timestamp", "code"}).AddRow(id, hash, height, at, code))
			_, e := s.applyCommissionSourceCorrections(apiMainnet, "payer", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{})
			require.ErrorContains(t, e, "identity changed")
			require.Empty(t, s.sourceCorrectionReceipts.receipts())
		})
	}
}
func TestCommissionSourceCorrectionPostgresTimestampPrecision(t *testing.T) {
	for _, rounded := range []bool{false, true} {
		t.Run(fmt.Sprint(rounded), func(t *testing.T) {
			p := correctionTestManifest(t)
			p.Fees[0].Timestamp = time.Date(2025, 1, 12, 0, 0, 5, 123456789, time.UTC)
			require.NoError(t, p.validate())
			c := p.Fees[0]
			indexed := c.Timestamp.Truncate(time.Microsecond)
			if rounded {
				indexed = c.Timestamp.Round(time.Microsecond)
			}
			start, end := c.Timestamp.Truncate(time.Second), c.Timestamp.Truncate(time.Second).Add(time.Second)
			s, m := correctionTestSQL(t, p)
			deferred, err := s.deferNativeCorrectionFee(apiMainnet, "payer", c.TxHash, indexed, start, end)
			require.NoError(t, err)
			require.True(t, deferred)
			m.ExpectQuery("SELECT x.id,x.hash").WillReturnRows(sqlmock.NewRows([]string{"id", "hash", "height", "timestamp", "code"}).AddRow(42, c.TxHash, c.Height, indexed, c.Code))
			m.ExpectQuery("SELECT a.address AS actor").WillReturnRows(sqlmock.NewRows([]string{"actor", "denom", "amount"}).AddRow("constituent", apiUatom, "10"))
			m.ExpectQuery("SELECT count.*tax_fee_grants").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			m.ExpectQuery("SELECT m.id FROM messages").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(43))
			out, err := s.applyCommissionSourceCorrections(apiMainnet, "payer", start, end, walletRows{})
			require.NoError(t, err)
			require.Len(t, out.Fees, 1)
			require.True(t, out.Fees[0].Ts.Equal(indexed), "derived flow uses the actual validated indexed timestamp")
			_, err = s.deferNativeCorrectionFee(apiMainnet, "payer", c.TxHash, indexed.Add(time.Second), start, end)
			require.Error(t, err)
			_, err = s.deferNativeCorrectionFee(apiMainnet, "payer", c.TxHash, indexed, end, end.Add(time.Second))
			require.Error(t, err)
		})
	}
}

func TestCommissionSourceCorrectionUnrankedLifecyclePhase(t *testing.T) {
	for _, tc := range []struct {
		name, denom, amount string
		reject              bool
	}{{"native", apiUatom, "8", true}, {"other_asset", "foreign", "8", false}, {"zero_native", apiUatom, "0", false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := correctionTestManifest(t)
			s, m := correctionTestSQL(t, p)
			c := p.Fees[0]
			expectCorrectionFee(m, c, "constituent", 0)
			rows := walletRows{Events: []tax.TaxableEvent{{Timestamp: c.Timestamp, BlockHeight: c.Height, MessageID: 0, Category: "complete_unbonding", Amount: tc.amount, Denom: tc.denom, ToAddr: "payer"}}}
			out, err := s.applyCommissionSourceCorrections(apiMainnet, "payer", c.Timestamp, c.Timestamp.Add(time.Second), rows)
			if tc.reject {
				require.ErrorContains(t, err, "lifecycle ordering is not proved")
				require.Empty(t, s.sourceCorrectionReceipts.receipts())
			} else {
				require.NoError(t, err)
				require.Len(t, out.Fees, 1)
				require.Equal(t, rows.Events, out.Events)
			}
		})
	}
}

func TestCommissionSourceCorrectionRewardUnrankedLifecyclePhase(t *testing.T) {
	p := correctionTestManifest(t)
	s, m := correctionTestSQL(t, p)
	c := p.Rewards[0]
	expectCorrectionTx(m, c.CommissionCorrectionIdentity)
	m.ExpectQuery("SELECT m.id,mt.message_type").WillReturnRows(sqlmock.NewRows([]string{"id", "message_type"}).AddRow(71, c.MessageType))
	rows := walletRows{Events: []tax.TaxableEvent{{Timestamp: c.Timestamp, BlockHeight: c.Height, MessageID: 0, Category: "complete_unbonding", Amount: "8", Denom: apiUatom, ToAddr: c.Recipient}}}
	_, err := s.applyCommissionSourceCorrections(apiMainnet, c.Recipient, c.Timestamp, c.Timestamp.Add(time.Second), rows)
	require.ErrorContains(t, err, "lifecycle ordering is not proved")
	require.Empty(t, s.sourceCorrectionReceipts.receipts())
}

func TestCommissionSourceCorrectionAnteMessageCannotBeInvented(t *testing.T) {
	p := correctionTestManifest(t)
	s, m := correctionTestSQL(t, p)
	c := p.Fees[0]
	expectCorrectionTx(m, c.CommissionCorrectionIdentity)
	m.ExpectQuery("SELECT a.address AS actor").WillReturnRows(sqlmock.NewRows([]string{"actor", "denom", "amount"}).AddRow("constituent", apiUatom, "10"))
	m.ExpectQuery("SELECT count.*tax_fee_grants").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	m.ExpectQuery("SELECT m.id FROM messages").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, err := s.applyCommissionSourceCorrections(apiMainnet, "payer", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{})
	require.ErrorContains(t, err, "ante message identity is unavailable")
	require.Empty(t, s.sourceCorrectionReceipts.receipts())
}

func TestCommissionSourceCorrectionMissingWrongPayerFeeIsNotCalledRemoved(t *testing.T) {
	p := correctionTestManifest(t)
	s, m := correctionTestSQL(t, p)
	c := p.Fees[0]
	expectCorrectionFee(m, c, "", 0)
	out, err := s.applyCommissionSourceCorrections("cosmoshub-4", "constituent", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{})
	require.NoError(t, err)
	require.Empty(t, out.Fees)
	effect := s.sourceCorrectionReceipts.receipts()[0].Effects[0]
	require.False(t, effect.OriginalActorFeePresent)
	require.Equal(t, "no_wrong_indexed_payer_fee_present", effect.Disposition)
}

func TestCommissionSourceCorrectionObservedEmptyIntersection(t *testing.T) {
	p := correctionTestManifest(t)
	s, _ := correctionTestSQL(t, p)
	rows := walletRows{Fees: []feeRec{{Hash: "unrelated", Amount: "123", Denom: apiUatom}}}
	out, e := s.applyCommissionSourceCorrections(apiMainnet, "unvisited-wallet", p.From, p.AsOf, rows)
	require.NoError(t, e)
	require.Equal(t, rows, out)
	receipts := s.sourceCorrectionReceipts.receipts()
	require.Len(t, receipts, 1)
	require.Equal(t, "not_applicable_to_this_observed_wallet_period", receipts[0].Status)
	require.Empty(t, receipts[0].Effects)
	require.Equal(t, p.digest, receipts[0].ProfileSHA256)
	require.NotContains(t, fmt.Sprint(receipts), "unvisited-wallet")
}
func TestCommissionSourceCorrectionOrdinaryPublicUnchanged(t *testing.T) {
	rows := walletRows{Fees: []feeRec{{Hash: "old", Amount: "4"}}}
	s := &Server{}
	out, e := s.applyCommissionSourceCorrections(apiMainnet, "payer", time.Now(), time.Now(), rows)
	require.NoError(t, e)
	require.Equal(t, rows, out)
	deferred, err := s.deferNativeCorrectionFee(apiMainnet, "payer", "old", time.Now(), time.Now(), time.Now())
	require.NoError(t, err)
	require.False(t, deferred)
	p := correctionTestManifest(t)
	s.commissionSourceCorrections = p
	_, e = s.applyCommissionSourceCorrections(apiMainnet, "payer", p.From, p.AsOf, rows)
	require.ErrorContains(t, e, "private snapshot")
}

func TestCommissionSourceCorrectionSixNativeRewardsAtReadTime(t *testing.T) {
	p := correctionTestManifest(t)
	s, m := correctionTestSQL(t, p)
	c := p.Rewards[0]
	for i := 0; i < 4; i++ {
		expectCorrectionTx(m, p.Rewards[i].CommissionCorrectionIdentity)
		m.ExpectQuery("SELECT m.id,mt.message_type").WillReturnRows(sqlmock.NewRows([]string{"id", "message_type"}).AddRow(71, c.MessageType))
	}
	out, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{})
	require.NoError(t, e)
	require.Len(t, out.CommissionCorrections, 4)
	require.Empty(t, out.Events)
	for i, event := range out.CommissionCorrections {
		require.Equal(t, uint(71), event.Key.Message)
		require.Equal(t, i, event.Key.Sub)
		require.Equal(t, event.Key, *s.sourceCorrectionReceipts.receipts()[0].Effects[i].ResolvedSourceKey)
		require.Equal(t, "2", event.Amount.String())
		require.Equal(t, "in", event.Dir)
		require.Equal(t, "native_source_correction", event.Trigger)
	}
	require.Len(t, s.sourceCorrectionReceipts.receipts()[0].Effects, 4)
}
func TestCommissionSourceCorrectionRewardRejectsUnprovedOrdering(t *testing.T) {
	p := correctionTestManifest(t)
	c := p.Rewards[0]
	rows := walletRows{Events: []tax.TaxableEvent{{TxHash: c.TxHash, MessageID: 71, SubIndex: 0, Category: apiTransfer, Amount: "4", Denom: apiUatom, FromAddr: "reward-a", ToAddr: "outside", Timestamp: c.Timestamp, BlockHeight: c.Height}}}
	fingerprint := correctionEventsSHA(rows.Events, c.TxHash, 71)
	for i := 0; i < 4; i++ {
		p.Rewards[i].OriginalMessageEventsSHA256 = fingerprint
	}
	s, m := correctionTestSQL(t, p)
	expectCorrectionTx(m, c.CommissionCorrectionIdentity)
	m.ExpectQuery("SELECT m.id,mt.message_type").WillReturnRows(sqlmock.NewRows([]string{"id", "message_type"}).AddRow(71, c.MessageType))
	_, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", c.Timestamp, c.Timestamp.Add(time.Second), rows)
	require.ErrorContains(t, e, "ordering is not proved")
	require.Empty(t, s.sourceCorrectionReceipts.receipts())
}
func TestCommissionSourceCorrectionRewardSourceChanged(t *testing.T) {
	p := correctionTestManifest(t)
	c := p.Rewards[0]
	s, m := correctionTestSQL(t, p)
	expectCorrectionTx(m, c.CommissionCorrectionIdentity)
	m.ExpectQuery("SELECT m.id,mt.message_type").WillReturnRows(sqlmock.NewRows([]string{"id", "message_type"}).AddRow(71, c.MessageType))
	_, e := s.applyCommissionSourceCorrections(apiMainnet, "reward-a", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{Events: []tax.TaxableEvent{{TxHash: c.TxHash, MessageID: 71, SubIndex: 0, Category: apiReward, Denom: apiUatom, Amount: "2", ToAddr: "reward-a"}}})
	require.ErrorContains(t, e, "footprint changed")
	require.Empty(t, s.sourceCorrectionReceipts.receipts())
}

func TestCommissionSourceCorrectionGenerationAndReceiptIsolation(t *testing.T) {
	p := correctionTestManifest(t)
	authz := &CommissionPrivateProfile{digest: p.BaseProfileSHA256, From: p.From, AsOf: p.AsOf}
	o := CommissionFlowBuildOptions{Version: "candidate-profile" + authz.digest[:12] + "-sources" + p.digest[:12], Build: "new-source-build", AsOf: p.AsOf, PrivateProfile: authz, SourceCorrections: p}
	require.NoError(t, o.validate())
	combined := o.profileSHA()
	require.NotEqual(t, p.digest, combined)
	require.NotEqual(t, authz.digest, combined)
	old := o
	old.SourceCorrections = nil
	require.Error(t, old.validate())
	old.Version = "candidate-profile" + authz.digest[:12]
	require.NoError(t, old.validate())
	require.NotEqual(t, combined, old.profileSHA())
	o.Version = "old-profile" + authz.digest[:12]
	require.Error(t, o.validate())
	o.SourceCorrections = p
	o.PrivateProfile = nil
	require.Error(t, o.validate())
	s := &Server{readOnlyLedger: true, commissionSourceCorrections: p, sourceCorrectionReceipts: newCorrectionJournal()}
	base := &dbFlowLoader{s: s, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}, private: true}
	a, la := s.privateProjectionTrace(base)
	b, lb := s.privateProjectionTrace(base)
	a.sourceCorrectionReceipts.retain(CommissionSourceCorrectionReceipt{ProfileSHA256: p.digest, WalletSHA256: privateSHA([]byte("a")), Status: "not_applicable_to_this_observed_wallet_period"})
	require.Len(t, la.evidence().SourceCorrectionReads, 1)
	require.Empty(t, lb.evidence().SourceCorrectionReads)
	require.Empty(t, b.sourceCorrectionReceipts.receipts())
	require.Empty(t, s.sourceCorrectionReceipts.receipts())
}
func TestCommissionSourceCorrectionJournalCommitAndRollback(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprint(commit), func(t *testing.T) {
			p := correctionTestManifest(t)
			s, m := correctionTestSQL(t, p)
			m.ExpectBegin()
			m.ExpectExec(regexp.QuoteMeta("SET LOCAL plan_cache_mode = force_custom_plan")).WillReturnResult(sqlmock.NewResult(0, 0))
			if commit {
				m.ExpectCommit()
			} else {
				m.ExpectRollback()
			}
			err := s.withReadSnapshot(func(reader *Server) error {
				reader.sourceCorrectionReceipts.retain(CommissionSourceCorrectionReceipt{ProfileSHA256: p.digest, Status: "test-only"})
				if !commit {
					return fmt.Errorf("source check failed")
				}
				return nil
			})
			if commit {
				require.NoError(t, err)
				require.Len(t, s.sourceCorrectionReceipts.receipts(), 1)
			} else {
				require.Error(t, err)
				require.Empty(t, s.sourceCorrectionReceipts.receipts())
			}
		})
	}
}

func TestCommissionSourceCorrectionOutsideScopeNativeRulesIdentical(t *testing.T) {
	p := correctionTestManifest(t)
	s, _ := correctionTestSQL(t, p)
	l := newFakeFlowLedger()
	l.openings[flowTestReceiver] = flowHoldings{Liquid: amt(50)}
	l.commission(flowAt(1, 0), flowTestReceiver, 100)
	l.send(flowAt(2, 0), flowTestReceiver, flowTestExchange, 120)
	before := runFlowTest(t, l, flowTestReceiver)
	rows := walletRows{Events: []tax.TaxableEvent{
		{Timestamp: flowAt(1, 0).Time, BlockHeight: flowAt(1, 0).Height, MessageID: flowAt(1, 0).Message, Category: apiCommission, Amount: "100", Denom: apiUatom, ToAddr: flowTestReceiver, ValidatorAddress: flowTestValidator},
		{Timestamp: flowAt(2, 0).Time, BlockHeight: flowAt(2, 0).Height, MessageID: flowAt(2, 0).Message, Category: apiTransfer, Amount: "120", Denom: apiUatom, FromAddr: flowTestReceiver, ToAddr: flowTestExchange},
	}}
	out, e := s.applyCommissionSourceCorrections(apiMainnet, flowTestReceiver, flowTestStart, flowTestStart.AddDate(0, 3, 0), rows)
	require.NoError(t, e)
	require.Equal(t, rows, out)
	corrected := newFakeFlowLedger()
	corrected.openings[flowTestReceiver] = l.openings[flowTestReceiver]
	corrected.rows[flowTestReceiver] = commissionFlowEventsFromRows(out, flowTestReceiver, apiUatom, nil)
	after := runFlowTest(t, corrected, flowTestReceiver)
	require.Equal(t, before.Fates, after.Fates)
	require.Equal(t, before.Commission, after.Commission)
	require.Equal(t, "70", fate(after, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "80", fate(after, flowRuleProRata, "2025-01", flowExchange, "Test Exchange", false))
}
func TestCommissionSourceCorrectionCorrectedRowsDriveBothNativeRules(t *testing.T) {
	p := correctionTestManifest(t)
	for i := range p.Fees {
		if i > 0 {
			p.Fees[i].Timestamp = flowTestStart.AddDate(0, 4, 0).Add(time.Duration(i) * time.Second)
		}
	}
	for i := range p.Rewards {
		p.Rewards[i].Recipient = "payer"
	}
	p.Windows[3].Wallet, p.Windows[4].Wallet = "payer", "payer"
	require.NoError(t, p.validate())
	s, m := correctionTestSQL(t, p)
	expectCorrectionFee(m, p.Fees[0], "constituent", 0)
	for _, c := range p.Rewards {
		expectCorrectionTx(m, c.CommissionCorrectionIdentity)
		m.ExpectQuery("SELECT m.id,mt.message_type").WillReturnRows(sqlmock.NewRows([]string{"id", "message_type"}).AddRow(71, c.MessageType))
	}
	rows := walletRows{Events: []tax.TaxableEvent{
		{Timestamp: flowTestStart, BlockHeight: 1, MessageID: 1, Category: apiCommission, Amount: "100", Denom: apiUatom, ToAddr: "payer", ValidatorAddress: flowTestValidator},
		{Timestamp: flowTestStart.AddDate(0, 1, 4), BlockHeight: 100000, MessageID: 999, Category: apiTransfer, Amount: "35", Denom: apiUatom, FromAddr: "payer", ToAddr: flowTestExchange},
	}}
	out, err := s.applyCommissionSourceCorrections(apiMainnet, "payer", flowTestStart, flowTestStart.AddDate(0, 3, 0), rows)
	require.NoError(t, err)
	require.Len(t, out.CommissionCorrections, 6)
	l := newFakeFlowLedger()
	l.rows["payer"] = commissionFlowEventsFromRows(out, "payer", apiUatom, nil)
	require.Len(t, l.rows["payer"], 9)
	tr := runFlowTest(t, l, "payer")
	require.Equal(t, "35", fate(tr, flowRuleFIFO, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "31", fate(tr, flowRuleProRata, "2025-01", flowExchange, "Test Exchange", false))
	require.Equal(t, "58", fate(tr, flowRuleFIFO, "2025-01", flowHeld, "", false))
	require.Equal(t, "62", fate(tr, flowRuleProRata, "2025-01", flowHeld, "", false))
	for rule := 0; rule < flowRules; rule++ {
		require.Equal(t, "7", fate(tr, rule, "2025-01", flowFees, "", false))
		sum := amt(0)
		for key, value := range tr.Fates {
			if key.Rule == rule {
				sum = sum.Add(value)
			}
		}
		require.Equal(t, "100", sum.String())
	}
}

func TestCommissionSourceCorrectionFeeFollowsPriorMessageInSameBlock(t *testing.T) {
	p := correctionTestManifest(t)
	c := p.Fees[0]
	rows := walletRows{Events: []tax.TaxableEvent{
		{Timestamp: c.Timestamp, BlockHeight: c.Height, MessageID: 40, TxHash: "prior-transaction", Category: apiCommission, Amount: "100", Denom: apiUatom, ToAddr: "payer", ValidatorAddress: flowTestValidator},
		{Timestamp: c.Timestamp, BlockHeight: c.Height, MessageID: 43, TxHash: c.TxHash, Category: apiTransfer, Amount: "50", Denom: apiUatom, FromAddr: "payer", ToAddr: flowTestExchange},
	}}
	p.Fees[0].ChargedActorEventsSHA256 = correctionEventsSHA(rows.Events, c.TxHash, 0)
	require.NoError(t, p.validate())
	s, m := correctionTestSQL(t, p)
	expectCorrectionFee(m, p.Fees[0], "constituent", 0)
	out, err := s.applyCommissionSourceCorrections(apiMainnet, "payer", c.Timestamp, c.Timestamp.Add(time.Second), rows)
	require.NoError(t, err)
	converted := commissionFlowEventsFromRows(out, "payer", apiUatom, nil)
	require.Len(t, converted, 3)
	require.Equal(t, apiCommission, converted[0].Category)
	require.Equal(t, apiFee, converted[1].Category)
	require.Equal(t, uint(43), converted[1].Key.Message)
	require.Equal(t, -1, converted[1].Key.Sub)
	l := newFakeFlowLedger()
	l.rows["payer"] = converted
	tr := runFlowTest(t, l, "payer")
	for rule := 0; rule < flowRules; rule++ {
		require.Equal(t, "7", fate(tr, rule, "2025-01", flowFees, "", false))
		require.Equal(t, "50", fate(tr, rule, "2025-01", flowExchange, "Test Exchange", false))
		require.Equal(t, "43", fate(tr, rule, "2025-01", flowHeld, "", false))
	}
}

func TestCommissionSourceCorrectionDefersActualIndexedFeeAtReader(t *testing.T) {
	for _, code := range []int64{0, 9} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			p := correctionTestManifest(t)
			p.Fees[0].Code = code
			s, m := correctionTestSQL(t, p)
			c := p.Fees[0]
			m.ExpectQuery(`SELECT id FROM "addresses"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(17))
			m.ExpectQuery(`SELECT.*FROM "chains"`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(18))
			m.ExpectQuery("attributed ORDER BY").WillReturnRows(sqlmock.NewRows([]string{"tx_id", "hash", "code", "denom", "amount", "ts", "resolved", "charged"}).AddRow(42, c.TxHash, code, apiUatom, "10", c.Timestamp, false, "").AddRow(43, "unrelated", 0, apiUatom, "5", c.Timestamp, false, ""))
			fees, err := s.effectiveWalletFees(apiMainnet, "constituent", c.Timestamp, c.Timestamp.Add(time.Second))
			require.NoError(t, err)
			require.Len(t, fees, 1)
			require.Equal(t, "unrelated", fees[0].Hash)
			expectCorrectionFee(m, c, "constituent", 0)
			out, err := s.applyCommissionSourceCorrections(apiMainnet, "constituent", c.Timestamp, c.Timestamp.Add(time.Second), walletRows{Fees: fees})
			require.NoError(t, err)
			require.Equal(t, fees, out.Fees)
			require.Equal(t, "removed_wrong_indexed_payer_fee", s.sourceCorrectionReceipts.receipts()[0].Effects[0].Disposition)
		})
	}
}
