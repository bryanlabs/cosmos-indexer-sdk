//go:build ops818dynamic0607v1 && ops818harness && ops818cohorts0607

package taxapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func dyFixture(t *testing.T) (*dyLoader, dyAdmission, dySourceProof, string, time.Time, time.Time) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("OPS818_DYNAMIC_ADMISSIONS", root)
	a := "synthetic-wallet"
	start := time.Date(2024, 10, 14, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 2)
	key := dyID(a, start, end)
	input := filepath.Join(root, "input.json")
	events := filepath.Join(root, "events.json")
	receipt := filepath.Join(root, "receipt.json")
	proofPath := filepath.Join(root, "proof.json")
	if e := dySave(receipt, map[string]any{"synthetic_receipt": true}); e != nil {
		t.Fatal(e)
	}
	rh, _ := dyHash(receipt)
	if e := dySave(input, reportInput{Wallets: []walletRows{{}}}); e != nil {
		t.Fatal(e)
	}
	ev := flowEvent{Key: flowKey{Time: start.Add(time.Hour)}}
	if e := dySave(events, []flowEvent{ev, ev}); e != nil {
		t.Fatal(e)
	}
	ih, _ := dyHash(input)
	eh, _ := dyHash(events)
	proof := dySourceProof{Source: dySource, Address: a, Since: start, End: end, Key: key, Input: input, InputSHA: ih, Events: events, EventsSHA: eh, Receipt: receipt, ReceiptSHA: rh, Raw: receipt, RawSHA: rh, Credits: receipt, CreditsSHA: rh, Build: receipt, BuildSHA: rh, Uniqueness: receipt, UniquenessSHA: rh, Finalizer: receipt, FinalizerSHA: rh, FlowCount: 2, NativeComplete: true, Readonly: "on", XID: json.RawMessage("null")}
	if e := dySave(proofPath, proof); e != nil {
		t.Fatal(e)
	}
	ph, _ := dyHash(proofPath)
	adm := dyAdmission{Source: dySource, Window: dyWindow{Key: key, Address: a, Since: start, End: end, Input: input, InputSHA: ih, Receipt: receipt, ReceiptSHA: rh}, Events: events, EventsSHA: eh, CompleteProof: proofPath, CompleteProofSHA: ph}
	if e := dySave(filepath.Join(root, key+".private.json"), adm); e != nil {
		t.Fatal(e)
	}
	l := &dyLoader{windows: map[string]dyWindow{}, used: map[string]string{}}
	return l, adm, proof, a, start, end
}
func dyRewriteFixture(t *testing.T, adm dyAdmission, proof dySourceProof) {
	t.Helper()
	b, _ := json.Marshal(proof)
	if proof.XID == nil {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(b, &fields)
		delete(fields, "assigned_xid")
		b, _ = json.Marshal(fields)
	}
	if e := os.WriteFile(adm.CompleteProof, b, 0600); e != nil {
		t.Fatal(e)
	}
	adm.CompleteProofSHA, _ = dyHash(adm.CompleteProof)
	b, _ = json.Marshal(adm)
	if e := os.WriteFile(filepath.Join(os.Getenv("OPS818_DYNAMIC_ADMISSIONS"), adm.Window.Key+".private.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestOPS818DynamicAdmissionGuards(t *testing.T) {
	t.Run("literal-duplicates-retained", func(t *testing.T) {
		l, _, _, a, s, e := dyFixture(t)
		events, ready, err := dyAdmissionEvents(l, a, s, e)
		if err != nil || !ready || len(events) != 2 || !dyEqual(events[0], events[1]) || len(l.manifest.Windows) != 1 {
			t.Fatal("lossless source admission failed")
		}
	})
	t.Run("channel-journal-failure-does-not-wait", func(t *testing.T) {
		oldRoot, oldRequest := dyRoot, dyRequestRoot
		defer func() { dyRoot, dyRequestRoot = oldRoot, oldRequest }()
		dyRoot = t.TempDir()
		dyRequestRoot = filepath.Join(t.TempDir(), "nonexistent")
		l := &dyLoader{}
		now := time.Now().UTC()
		tr := newFlowTrace("synthetic-validator", now, now.Add(time.Hour), l)
		event := flowEvent{Category: flowCatIBCOut, Dir: apiOut, Amount: decimal.NewFromInt(1), Detail: `{"packet_src_port":"transfer","packet_src_channel":"synthetic-unbound"}`, Key: flowKey{Time: now}}
		tr.queue = flowQueue{flowQueued{event: event, seq: 1, wallet: &flowWallet{Addr: "synthetic-wallet"}}}
		_, phase, err := dyRun(tr, t.TempDir())
		if phase != "channel-request-journal-failed" || err == nil {
			t.Fatal("failed journal entered wait")
		}
	})
	cases := []string{"source", "address", "since", "end", "key", "input-hash", "events-hash", "incomplete", "readonly", "assigned-xid", "missing-xid"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			l, adm, p, a, s, e := dyFixture(t)
			switch name {
			case "source":
				p.Source = "wrong"
			case "address":
				p.Address = "wrong"
			case "since":
				p.Since = p.Since.Add(time.Second)
			case "end":
				p.End = p.End.Add(time.Second)
			case "key":
				p.Key = "wrong"
			case "input-hash":
				p.InputSHA = "wrong"
			case "events-hash":
				p.EventsSHA = "wrong"
			case "incomplete":
				p.NativeComplete = false
			case "readonly":
				p.Readonly = "off"
			case "assigned-xid":
				p.XID = json.RawMessage("123")
			case "missing-xid":
				p.XID = nil
			}
			dyRewriteFixture(t, adm, p)
			if _, ready, err := dyAdmissionEvents(l, a, s, e); err == nil || ready {
				t.Fatal("bad proof accepted")
			}
		})
	}
	t.Run("future-wait-releases-only-proved-input", func(t *testing.T) {
		l, _, _, a, s, e := dyFixture(t)
		oldRoot, oldReq := dyRoot, dyRequestRoot
		defer func() { dyRoot, dyRequestRoot = oldRoot, oldReq }()
		dyRoot = t.TempDir()
		dyRequestRoot = t.TempDir()
		l.pendingKind = "new-exact-native-input-and-bank"
		l.pendingDetails = map[string]any{"address": a, "since": s, "asof_exclusive": e, "canonical_window": dyID(a, s, e)}
		if err := dyWait(l); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("admission-mutation-after-use-rejected", func(t *testing.T) {
		l, adm, p, a, s, e := dyFixture(t)
		if _, _, err := dyAdmissionEvents(l, a, s, e); err != nil {
			t.Fatal(err)
		}
		p.NativeComplete = false
		dyRewriteFixture(t, adm, p)
		if _, ready, err := dyAdmissionEvents(l, a, s, e); err == nil || ready {
			t.Fatal("mutable admission accepted")
		}
	})
	t.Run("retained-window-conflict-rejected", func(t *testing.T) {
		l, adm, _, a, s, e := dyFixture(t)
		w := adm.Window
		w.ReceiptSHA = "different"
		l.windows[w.Key] = w
		if _, ready, err := dyAdmissionEvents(l, a, s, e); err == nil || ready {
			t.Fatal("conflicting source accepted")
		}
	})
}
