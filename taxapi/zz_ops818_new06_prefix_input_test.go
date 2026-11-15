//go:build ops818new06input && ops818opening06storedv3 && ops818harness && ops818cohorts0607

package taxapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var n6Dir = os.Getenv("OPS818_NEW06_ROOT")

type n6Scope struct {
	Source, Address, CanonicalWindow, TailInput, TailInputSHA, TailReceipt, TailReceiptSHA, SuffixCredits, SuffixCreditsSHA, UniquenessProof, UniquenessSHA string
	Since, End, PrefixEnd                                                                                                                                   time.Time
	FingerprintContract                                                                                                                                     bool
}
type n6CreditRow struct {
	EventID, MessageID, ReceiverAttributeID, AmountAttributeID uint
	Timestamp                                                  time.Time
	Receiver, Amount                                           string
	ReceiverIndex, AmountIndex                                 int
}

func n6Join(prefix, tail reportInput, s n6Scope) (reportInput, error) {
	if len(prefix.Wallets) != 1 || len(tail.Wallets) != 1 || prefix.Fingerprint != "" || tail.Fingerprint != "" || s.FingerprintContract {
		return reportInput{}, errors.New("full input/fingerprint contract mismatch")
	}
	for i, p := range []reportInput{prefix, tail} {
		a, z := s.Since, s.PrefixEnd
		if i == 1 {
			a, z = s.PrefixEnd, s.End
		}
		for _, r := range p.Wallets[0].Events {
			if r.Timestamp.Before(a) || !r.Timestamp.Before(z) {
				return reportInput{}, errors.New("source event outside disjoint part")
			}
		}
		for _, r := range p.Wallets[0].Fees {
			if r.Ts.Before(a) || !r.Ts.Before(z) {
				return reportInput{}, errors.New("source fee outside disjoint part")
			}
		}
	}
	out := reportInput{Wallets: []walletRows{{Events: append(prefix.Wallets[0].Events, tail.Wallets[0].Events...), Fees: append(prefix.Wallets[0].Fees, tail.Wallets[0].Fees...), Memos: map[string]string{}}}, Fingerprint: tail.Fingerprint}
	for _, p := range []reportInput{prefix, tail} {
		for k, v := range p.Wallets[0].Memos {
			if old, ok := out.Wallets[0].Memos[k]; ok && old != v {
				return reportInput{}, errors.New("source memo conflict")
			}
			out.Wallets[0].Memos[k] = v
		}
	}
	return out, nil
}
func n6Credits(rows []n6CreditRow, s n6Scope) (map[flowCredit]bool, error) {
	credits := map[flowCredit]bool{}
	for _, r := range rows {
		if r.Receiver != s.Address || r.Timestamp.Before(s.Since) || !r.Timestamp.Before(s.PrefixEnd) {
			return nil, errors.New("raw native credit outside novel prefix/actor")
		}
		for _, coin := range strings.Split(r.Amount, ",") {
			credits[flowCredit{r.MessageID, strings.TrimSpace(coin)}] = true
		}
	}
	return credits, nil
}
func TestOPS818New06PrefixInput(t *testing.T) {
	if n6Dir == "" {
		t.Skip("private exact new06 capture only")
	}
	var scope n6Scope
	if e := v6Read(filepath.Join(n6Dir, "scope.private.json"), &scope); e != nil || scope.Source != xcSource || !scope.Since.Before(scope.PrefixEnd) || !scope.PrefixEnd.Before(scope.End) {
		t.Fatal("source scope invalid")
	}
	head, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(head)) != xcSource {
		t.Fatal("current source HEAD mismatch")
	}
	cmd := exec.Command("git", "diff", "--quiet", xcSource, "--", "taxapi", ":(exclude)taxapi/zz_*")
	if cmd.Run() != nil {
		t.Fatal("production source changed")
	}
	for path, want := range map[string]string{scope.TailInput: scope.TailInputSHA, scope.TailReceipt: scope.TailReceiptSHA, scope.SuffixCredits: scope.SuffixCreditsSHA, scope.UniquenessProof: scope.UniquenessSHA} {
		h, e := xcHash(path)
		if e != nil || h != want {
			t.Fatal("retained source hash mismatch")
		}
	}
	if xcID(scope.Address, scope.Since, scope.End) != scope.CanonicalWindow {
		t.Fatal("canonical requested window mismatch")
	}
	var unique struct {
		Source  string
		Request struct {
			Source  string
			Request struct {
				Address string    `json:"address"`
				Since   time.Time `json:"since"`
				End     time.Time `json:"asof_exclusive"`
				Key     string    `json:"canonical_window"`
			} `json:"request"`
		} `json:"request"`
	}
	if b, e := os.ReadFile(scope.UniquenessProof); e != nil || json.Unmarshal(b, &unique) != nil || unique.Source != scope.Source || unique.Request.Source != scope.Source || unique.Request.Request.Address != scope.Address || !unique.Request.Request.Since.Equal(scope.Since) || !unique.Request.Request.End.Equal(scope.End) || unique.Request.Request.Key != scope.CanonicalWindow {
		t.Fatal("literal actual request/source uniqueness mismatch")
	}
	var tail reportInput
	if v6Read(scope.TailInput, &tail) != nil {
		t.Fatal("retained full tail unavailable")
	}
	rec := &v6Log{Interface: logger.Default.LogMode(logger.Silent)}
	db, e := gorm.Open(postgres.Open("host=127.0.0.1 port=15439 user=postgres dbname=cosmostax sslmode=disable application_name=ops818-private-new06-prefix default_transaction_read_only=on"), &gorm.Config{Logger: rec})
	if e != nil {
		t.Fatal("ROsource transport failed")
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal("pool unavailable")
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	s := &Server{db: db, native: DefaultNativeAsset, readOnlyLedger: true, privateReadSnapshotTimeout: time.Minute}
	var prefix reportInput
	var rawCredits []n6CreditRow
	var controls v6Controls
	began := time.Now()
	fmt.Printf("phase new06_fresh_prefix_read_started count 1\n")
	e = s.withReadSnapshot(func(reader *Server) error {
		if e := reader.db.Exec("SET LOCAL statement_timeout='60s'").Error; e != nil {
			return e
		}
		if e := reader.db.Exec("SET LOCAL lock_timeout='2s'").Error; e != nil {
			return e
		}
		control := func() error {
			if e := reader.db.Raw("SELECT current_setting('transaction_read_only') AS readonly,txid_current_if_assigned() AS assigned_x_id,txid_current_snapshot()::text AS snapshot,current_database() AS database").Scan(&controls).Error; e != nil {
				return e
			}
			if controls.Readonly != "on" || controls.AssignedXID != nil || controls.Database != "cosmostax" {
				return errors.New("fresh source controls rejected")
			}
			return nil
		}
		if e := control(); e != nil {
			return e
		}
		// Preserve the true final-AsOf refund dependencies, not prefix-AsOf ones.
		refunds, e := reader.reportReadiness(apiMainnet, []string{scope.Address}, scope.Since, scope.End)
		if e != nil {
			return e
		}
		rows, e := reader.loadWalletRows(apiMainnet, scope.Address, scope.Since, scope.PrefixEnd, refunds)
		if e != nil {
			return e
		}
		prefix = reportInput{Wallets: []walletRows{rows}, Fingerprint: ""}
		ids := []uint{}
		seen := map[uint]bool{}
		for _, r := range rows.Events {
			if flowSingleHopNative(r, DefaultNativeAsset.Denom) && r.ToAddr == scope.Address && !seen[r.MessageID] {
				ids = append(ids, r.MessageID)
				seen[r.MessageID] = true
			}
		}
		for from := 0; from < len(ids); from += 5000 {
			var raw []n6CreditRow
			// Exact original flowReturnCredits receiver/amount joins, retaining actual
			// identifiers and literal attributes instead of gross tax/NFT amounts.
			e = reader.db.Raw(`SELECT me.id AS event_id,me.message_id,b.time_stamp AS timestamp,a1.id AS receiver_attribute_id,a2.id AS amount_attribute_id,a1.value AS receiver,a2.value AS amount,a1.index AS receiver_index,a2.index AS amount_index
FROM message_events me JOIN message_event_types mt ON mt.id=me.message_event_type_id AND mt.type='coin_received'
JOIN message_event_attributes a1 ON a1.message_event_id=me.id JOIN message_event_attribute_keys k1 ON k1.id=a1.message_event_attribute_key_id AND k1.key='receiver'
JOIN message_event_attributes a2 ON a2.message_event_id=me.id JOIN message_event_attribute_keys k2 ON k2.id=a2.message_event_attribute_key_id AND k2.key='amount'
JOIN messages m ON m.id=me.message_id JOIN txes t ON t.id=m.tx_id JOIN blocks b ON b.id=t.block_id
WHERE me.message_id IN ? AND a1.value=? ORDER BY me.id,a1.index,a2.index`, ids[from:min(from+5000, len(ids))], scope.Address).Scan(&raw).Error
			if e != nil {
				return e
			}
			rawCredits = append(rawCredits, raw...)
		}
		if _, e = n6Credits(rawCredits, scope); e != nil {
			return e
		}
		return control()
	})
	if e != nil {
		_ = v6Write(filepath.Join(n6Dir, "fresh-prefix-failure.private.json"), map[string]any{"source": xcSource, "error": e.Error(), "controls": controls, "queries": rec.Queries, "elapsed_ms": time.Since(began).Milliseconds(), "financially_accepted": false}, false)
		t.Fatal("fresh prefix capture failed explicitly")
	}
	if e = v6Write(filepath.Join(n6Dir, "fresh-prefix.input.private.json"), prefix, false); e != nil {
		t.Fatal("prefix input save failed")
	}
	if e = v6Write(filepath.Join(n6Dir, "fresh-prefix.credit-attributes.private.json"), rawCredits, false); e != nil {
		t.Fatal("prefix raw credits save failed")
	}
	joined, e := n6Join(prefix, tail, scope)
	if e != nil {
		t.Fatal("disjoint full input join rejected")
	}
	creditMap, e := n6Credits(rawCredits, scope)
	if e != nil {
		t.Fatal("prefix raw credit decode rejected")
	}
	var suffix struct {
		Credits struct {
			Records []struct {
				Message uint   `json:"message"`
				Coin    string `json:"coin"`
			} `json:"credits"`
		} `json:"credits"`
		Verified bool `json:"verified_against_actual_bank_attributes"`
	}
	if v6Read(scope.SuffixCredits, &suffix) != nil || !suffix.Verified {
		t.Fatal("retained suffix credit proof unavailable")
	}
	for _, r := range suffix.Credits.Records {
		creditMap[flowCredit{r.Message, r.Coin}] = true
	}
	events, e := xcConvert(joined, scope.Address, scope.Since, scope.End, creditMap)
	if e != nil {
		t.Fatal("joined native event projection rejected")
	}
	inputPath := filepath.Join(n6Dir, "joined-full-current6754.input.private.json")
	eventsPath := filepath.Join(n6Dir, "joined-native.events.private.json")
	if e = v6Write(inputPath, joined, false); e != nil {
		t.Fatal("full input save failed")
	}
	if e = v6Write(eventsPath, events, false); e != nil {
		t.Fatal("native event projection save failed")
	}
	hashes := map[string]string{}
	for _, name := range []string{"server.go", "read_snapshot.go", "commissionflows.go", "zz_ops818_new06_prefix_input_test.go", "zz_ops818_cohorts0607_private_test.go"} {
		h, e := xcHash(filepath.Join("/Users/danb/code/worktrees/cosmos-tax-commission-route-8704/taxapi", name))
		if e != nil {
			t.Fatal("source binding unavailable")
		}
		hashes[name] = h
	}
	ih, _ := xcHash(inputPath)
	eh, _ := xcHash(eventsPath)
	ph, _ := xcHash(filepath.Join(n6Dir, "fresh-prefix.input.private.json"))
	ch, _ := xcHash(filepath.Join(n6Dir, "fresh-prefix.credit-attributes.private.json"))
	if e = v6Write(filepath.Join(n6Dir, "complete-source-proof.private.json"), map[string]any{"source": xcSource, "scope": scope, "controls": controls, "queries": rec.Queries, "code_sha256": hashes, "fresh_prefix_input_sha256": ph, "fresh_prefix_native_attributes_sha256": ch, "joined_input_sha256": ih, "events_sha256": eh, "prefix_event_rows": len(prefix.Wallets[0].Events), "tail_event_rows": len(tail.Wallets[0].Events), "joined_event_rows": len(joined.Wallets[0].Events), "native_flow_events": len(events), "fresh_prefix_same_snapshot": true, "joined_input_same_snapshot": false, "overlap_recaptured": false, "event_occurrences_deduplicated": false, "elapsed_ms": time.Since(began).Milliseconds(), "financially_accepted": false}, false); e != nil {
		t.Fatal("complete source proof save failed")
	}
	fmt.Printf("phase new06_full_current_input_proved count %d native %d proof_sha256 %s\n", len(joined.Wallets[0].Events), len(events), ih)
}
