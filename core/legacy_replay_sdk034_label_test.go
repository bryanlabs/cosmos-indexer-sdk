package core

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	distr "github.com/cosmos/cosmos-sdk/x/distribution/types"
)

// The separate replay034 module checks this literal against the actual v0.34.10
// MsgSetWithdrawAddress.Route/Type methods, not the modern derived message.
func TestReplaySDK034WithdrawAddressExactLabel(t *testing.T) {
	msg := legacy.Message{AminoName: "cosmos-sdk/MsgModifyWithdrawAddress", Derived: &distr.MsgSetWithdrawAddress{}}
	for _, label := range []string{"distr/set_withdraw_address", "distribution/set_withdraw_address", "unknown/set_withdraw_address", "Distr/set_withdraw_address", "distr/Set_withdraw_address", "bank/send", ""} {
		_, err := replayMessage(msg, []rr.Record{{Kind: "accounting", Status: "committed", Label: label}})
		if (err == nil) != (label == "distr/set_withdraw_address") {
			t.Fatal("exact SDK034 label admission differs from pinned route")
		}
	}
	h, items, records := replayFixture(t, 0, true)
	for i := range records.Records {
		if records.Records[i].Phase == "message" && records.Records[i].Kind == "accounting" {
			records.Records[i].Label = "distr/set_withdraw_address"
		}
	}
	block, err := LegacyReplayBlock(h, items, records)
	if err == nil || len(block.Rows) != 0 {
		t.Fatal("correct withdraw label accepted for a bank message")
	}
}

func TestReplaySDK034FailedAbortedMoneyRemainsEvidence(t *testing.T) {
	h, items, records := replayFixture(t, 5, true)
	block, err := LegacyReplayBlock(h, items, records)
	if err != nil || len(block.Rows) != 0 {
		t.Fatal("aborted money produced rows or lost supported failed transaction")
	}
	var kept []rr.Record
	if json.Unmarshal([]byte(block.Events[0].Attributes[0][1]), &kept) != nil || len(kept) != len(records.Records) {
		t.Fatal("aborted source evidence not retained")
	}
	for i := range records.Records {
		if records.Records[i].Phase == "message" && records.Records[i].Kind == "movement" {
			records.Records[i].Status = "committed"
			break
		}
	}
	block, err = LegacyReplayBlock(h, items, records)
	if err == nil || len(block.Rows) != 0 {
		t.Fatal("committed failed money accepted")
	}
}

// Private integration consumes the actual immutable bytes from the refused
// e8d1036 attempt. It never replays native blocks or prints rows or identities.
func TestPrivateSDK034ActualWithdrawAddressHeights(t *testing.T) {
	root := os.Getenv("HUB2_ACTUAL_RETAINED_WINDOW")
	if root == "" {
		t.Skip("private retained window not supplied")
	}
	raw, err := os.ReadFile(root + "/proof.json")
	if err != nil {
		t.Fatal("private proof unavailable")
	}
	var proof rr.Proof
	if json.Unmarshal(raw, &proof) != nil {
		t.Fatal("private proof unreadable")
	}
	f, err := os.Open(root + "/window.movements.ndjson")
	if err != nil {
		t.Fatal("private records unavailable")
	}
	defer f.Close()
	wanted := map[int64]bool{400391: true, 427134: true, 432459: true, 432711: true, 432735: true, 432750: true, 436128: true, 461762: true, 469288: true, 469770: true, 469806: true, 470012: true, 470034: true, 470878: true, 472126: true, 472203: true, 472694: true, 472801: true, 473594: true, 474097: true}
	checked, held, markers := 0, 0, 0
	err = rr.Stream(f, proof, func(r rr.Height) error {
		if !wanted[r.Number] {
			return nil
		}
		checked++
		for _, record := range r.Records {
			if record.Kind == "accounting" && record.Phase == "message" && record.Label == "distr/set_withdraw_address" && record.Code == 0 {
				markers++
			}
		}
		h, err := capture.LoadHeight(root+"/cache", "cosmoshub-2", r.Number, 64<<20)
		if err != nil {
			t.Fatal("actual raw cache load refused")
		}
		items, err := DecodeLegacyBlock(h.Block, h.Results)
		if err != nil {
			t.Fatal("actual signed messages refused")
		}
		if _, err = LegacyReplayBlock(h, items, r); err != nil {
			held++
		}
		for _, label := range []string{"distribution/set_withdraw_address", "unknown/set_withdraw_address", "Distr/set_withdraw_address"} {
			bad := rr.Height{Number: r.Number, Records: append([]rr.Record(nil), r.Records...)}
			for i := range bad.Records {
				if bad.Records[i].Label == "distr/set_withdraw_address" {
					bad.Records[i].Label = label
				}
			}
			if block, e := LegacyReplayBlock(h, items, bad); e == nil || len(block.Rows) != 0 {
				t.Fatal("actual-byte unknown or case-shifted label accepted")
			}
		}
		for i, record := range r.Records {
			if record.Kind == "movement" && record.Phase == "ante" && record.Status == "committed" {
				bad := rr.Height{Number: r.Number, Records: append([]rr.Record(nil), r.Records[:i]...)}
				bad.Records = append(bad.Records, r.Records[i+1:]...)
				if block, e := LegacyReplayBlock(h, items, bad); e == nil || len(block.Rows) != 0 {
					t.Fatal("actual-byte missing fee movement accepted")
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal("actual retained record bytes refused")
	}
	t.Logf("actual retained regression checked=%d markers=%d held=%d", checked, markers, held)
	if checked != 20 || markers != 20 || held != 0 {
		t.Fatal("actual SDK034 withdraw-address heights do not reconcile")
	}
}
