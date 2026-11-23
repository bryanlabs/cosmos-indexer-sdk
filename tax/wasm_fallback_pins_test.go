package tax

import (
	"regexp"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

// Every pin on a code with a verified release build is in the registry with
// the kind that build implies, and every such build names a 32-byte sha256.
func TestFallbackCodeBuildPins(t *testing.T) {
	kinds := map[uint64]string{192: contractRoleDAOStaking, 36: contractRoleDAOStaking, 689: contractRoleVault}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	pinned := map[uint64]int{}
	for addr, ev := range fallbackEvidence {
		b, ok := fallbackCodeBuilds[ev.codeID]
		if !ok {
			continue
		}
		if !sha.MatchString(b.checksum) || b.build == "" {
			t.Errorf("code %d: build evidence %q is incomplete", ev.codeID, b.build)
		}
		k, ok := knownContractsByAddress[addr]
		if !ok {
			t.Errorf("code %d pin is not in the registry", ev.codeID)
			continue
		}
		if k.Kind != kinds[ev.codeID] || k.FirstSeenHeight <= 0 {
			t.Errorf("code %d pin: kind %q first seen %d", ev.codeID, k.Kind, k.FirstSeenHeight)
		}
		pinned[ev.codeID]++
	}
	want := map[uint64]int{192: 6, 36: 1, 689: 1}
	for code, n := range want {
		if pinned[code] != n {
			t.Errorf("code %d: %d pins, want %d", code, pinned[code], n)
		}
	}
}

// The pinned staking modules and shares converter decide a wallet's
// unexplained coins: a stake is a deposit, a conversion is a swap, and a
// one-way coin through the converter stays a flagged transfer.
func TestFallbackCodeBuildClasses(t *testing.T) {
	pin := func(code uint64) string {
		for addr, ev := range fallbackEvidence {
			if ev.codeID == code {
				return addr
			}
		}
		t.Fatalf("no pin for code %d", code)
		return ""
	}
	signer := testBech(t, "signer", 20)
	staking, staking36, converter := pin(192), pin(36), pin(689)
	const token, oldShare, newShare = "factory/test/token", "factory/test/old", "factory/test/new"
	cases := []struct {
		name     string
		contract string
		evs      []txlog.LogMessageEvent
		want     string
	}{
		{"stake on a 192 module is a deposit", staking, []txlog.LogMessageEvent{transferEv(signer, staking, "30"+token)},
			fallbackLine(string(CategoryProtocolDeposit), "30", token, signer, staking, staking, false)},
		{"stake on a 36 module is a deposit", staking36, []txlog.LogMessageEvent{transferEv(signer, staking36, "12"+token)},
			fallbackLine(string(CategoryProtocolDeposit), "12", token, signer, staking36, staking36, false)},
		{"shares in and other shares back are a swap", converter, []txlog.LogMessageEvent{transferEv(signer, converter, "50"+oldShare), transferEv(converter, signer, "49"+newShare)},
			fallbackLine(string(CategorySwap), "50", oldShare, signer, converter, converter, false) + fallbackLine(string(CategorySwap), "49", newShare, converter, signer, converter, false)},
		{"shares only out of the converter stay flagged", converter, []txlog.LogMessageEvent{transferEv(converter, signer, "49"+newShare)},
			fallbackLine(string(CategoryTransfer), "49", newShare, converter, signer, converter, true)},
	}
	for _, c := range cases {
		if got := summary(parseExecute(t, signer, executeLog(c.contract, c.evs...))); got != c.want {
			t.Errorf("%s:\ngot:\n%swant:\n%s", c.name, got, c.want)
		}
	}
}
