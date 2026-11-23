package tax

import (
	"strings"
	"testing"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

// registerKind adds a contract of a kind to the registry for one test, with
// node evidence for it when evidence is set.
func registerKind(t *testing.T, addr, kind string, evidence bool) {
	t.Helper()
	knownContractsByAddress[addr] = KnownContract{Address: addr, Protocol: "Test", Name: "test " + kind, Kind: kind}
	if evidence {
		fallbackEvidence[addr] = struct {
			codeID uint64
			label  string
		}{1, "test " + kind}
	}
	t.Cleanup(func() {
		delete(knownContractsByAddress, addr)
		delete(fallbackEvidence, addr)
	})
}

func fallbackLine(category, amount, denom, from, to, contract string, review bool) string {
	extra := map[string]string{detailFallback: fallbackBank}
	if review {
		extra[detailReviewRequired] = attributeTrue
		extra[detailReviewReason] = ReviewUnclassifiedContractMovement
	}
	return strings.Join([]string{category, amount, denom, "", from, to, "", "", contractDetail(contract, extra)}, "|") + "\n"
}

// Each class of contract a wallet moves an unexplained coin with gets the
// rows its rule decides, against the contract; an undecided one is a
// transfer flagged for review. Native fallback rows are exactly what they
// always were, and a wallet's other coins in a message where it also has a
// native fallback row are flagged.
func TestWasmFallbackClasses(t *testing.T) {
	signer := testBech(t, "signer", 20)
	unknown, router, vault := testBech(t, "unknown", 32), testBech(t, "router", 32), testBech(t, "vault", 32)
	staking, rewards, bare := testBech(t, "staking", 32), testBech(t, "rewards", 32), testBech(t, "bare", 32)
	registerKind(t, router, contractRoleRouter, true)
	registerKind(t, vault, contractRoleVault, true)
	registerKind(t, staking, contractRoleDAOStaking, true)
	registerKind(t, rewards, attributeRewards, true)
	registerKind(t, bare, contractRoleRouter, false)
	const token, share, ibcA, ibcB = "factory/test/token", "factory/test/share", "ibc/AAA", "ibc/BBB"
	swap, transfer := string(CategorySwap), string(CategoryTransfer)
	cases := []struct {
		name string
		log  *txlog.LogMessage
		want string
	}{
		{"native only stays a plain transfer", executeLog(unknown, transferEv(signer, unknown, "100uatom")),
			fallbackLine(transfer, "100", "uatom", signer, unknown, unknown, false)},
		{"unknown contract: native row unchanged, other coin flagged after it", executeLog(unknown, transferEv(signer, unknown, "100uatom"), transferEv(unknown, signer, "7"+token)),
			fallbackLine(transfer, "100", "uatom", signer, unknown, unknown, false) + fallbackLine(transfer, "7", token, unknown, signer, unknown, true)},
		{"router with a native leg: native row unchanged, other coin flagged", executeLog(router, transferEv(signer, router, "100uatom"), transferEv(router, signer, "50"+ibcA)),
			fallbackLine(transfer, "100", "uatom", signer, router, router, false) + fallbackLine(transfer, "50", ibcA, router, signer, router, true)},
		{"router swap of two other coins", executeLog(router, transferEv(signer, router, "9"+ibcA), transferEv(router, signer, "4"+ibcB)),
			fallbackLine(swap, "9", ibcA, signer, router, router, false) + fallbackLine(swap, "4", ibcB, router, signer, router, false)},
		{"vault: other coins in for shares is a swap", executeLog(vault, transferEv(signer, vault, "100"+ibcA), transferEv(vault, signer, "100"+share)),
			fallbackLine(swap, "100", ibcA, signer, vault, vault, false) + fallbackLine(swap, "100", share, vault, signer, vault, false)},
		{"DAO staking: tokens in are a deposit", executeLog(staking, transferEv(signer, staking, "30"+token)),
			fallbackLine(string(CategoryProtocolDeposit), "30", token, signer, staking, staking, false)},
		{"DAO staking: tokens back are a withdrawal", executeLog(staking, transferEv(staking, signer, "30"+token)),
			fallbackLine(string(CategoryProtocolWithdrawal), "30", token, staking, signer, staking, false)},
		{"rewards distributor: coins received are income", executeLog(rewards, transferEv(rewards, signer, "12"+ibcA)),
			fallbackLine(string(CategoryIncome), "12", ibcA, rewards, signer, rewards, false)},
		{"rewards distributor: funding it is flagged", executeLog(rewards, transferEv(signer, rewards, "12"+ibcA)),
			fallbackLine(transfer, "12", ibcA, signer, rewards, rewards, true)},
		{"router: coins only paid are flagged", executeLog(router, transferEv(signer, router, "5"+ibcA)),
			fallbackLine(transfer, "5", ibcA, signer, router, router, true)},
		{"vault: shares only received are flagged", executeLog(vault, transferEv(vault, signer, "5"+share)),
			fallbackLine(transfer, "5", share, vault, signer, vault, true)},
		{"a registered kind without node evidence is flagged", executeLog(bare, transferEv(signer, bare, "9"+ibcA), transferEv(bare, signer, "4"+ibcB)),
			fallbackLine(transfer, "9", ibcA, signer, bare, bare, true) + fallbackLine(transfer, "4", ibcB, bare, signer, bare, true)},
		{"a swap through an unregistered contract is flagged", executeLog(router, transferEv(signer, router, "9"+ibcA),
			ev("execute", [2]string{attributeContractAddress, unknown}), transferEv(unknown, signer, "4"+ibcB)),
			fallbackLine(transfer, "9", ibcA, signer, router, router, true) + fallbackLine(transfer, "4", ibcB, unknown, signer, unknown, true)},
		{"a coin paid and returned in full adds nothing", executeLog(unknown, transferEv(signer, unknown, "100uatom"), transferEv(signer, unknown, "8"+token), transferEv(unknown, signer, "8"+token)),
			fallbackLine(transfer, "100", "uatom", signer, unknown, unknown, false)},
	}
	for _, c := range cases {
		if got := summary(parseExecute(t, signer, c.log)); got != c.want {
			t.Errorf("%s:\ngot:\n%swant:\n%s", c.name, got, c.want)
		}
	}
}

// Native rows keep their content and positions; other coins follow every
// native row.
func TestWasmFallbackKeepsNativePositions(t *testing.T) {
	signer, payee, contract := testBech(t, "signer", 20), testBech(t, "payee", 20), testBech(t, "contract", 32)
	log := executeLog(contract,
		transferEv(signer, contract, "100uatom"),
		transferEv(contract, payee, "40uatom"),
		transferEv(contract, signer, "3factory/test/token"))
	want := fallbackLine(string(CategoryTransfer), "100", "uatom", signer, contract, contract, false) +
		fallbackLine(string(CategoryTransfer), "40", "uatom", contract, payee, contract, false) +
		fallbackLine(string(CategoryTransfer), "3", "factory/test/token", contract, signer, contract, true)
	if got := summary(parseExecute(t, signer, log)); got != want {
		t.Fatalf("got:\n%swant:\n%s", got, want)
	}
}

// A coin other than the native one that a known row already names for the
// wallet gets no fallback row; another coin of the same wallet still does.
func TestWasmFallbackNamedPerDenom(t *testing.T) {
	signer, contract := testBech(t, "signer", 20), testBech(t, "contract", 32)
	log := executeLog(contract, transferEv(signer, contract, "9ibc/AAA"), transferEv(contract, signer, "4ibc/BBB"))
	known := []TaxableEvent{{Category: string(CategorySwap), FromAddr: signer, ToAddr: contract, Amount: "9", Denom: "ibc/AAA"}}
	got := summary(bankFallbackEvents(log, signer, known))
	want := fallbackLine(string(CategoryTransfer), "4", "ibc/BBB", contract, signer, contract, true)
	if got != want {
		t.Fatalf("got:\n%swant:\n%s", got, want)
	}
}

// A flattened log, with no signer, books other coins of 20-byte wallets all
// the same.
func TestWasmFallbackFlattenedOtherCoins(t *testing.T) {
	signer, other, contract := testBech(t, "signer", 20), testBech(t, "other", 20), testBech(t, "contract", 32)
	log := flatten(executeLog(contract, transferEv(signer, contract, "9ibc/AAA"), transferEv(contract, other, "4ibc/AAA")))
	rows := parseExecute(t, signer, log)
	if len(rows) != 2 {
		t.Fatalf("flattened: %s", summary(rows))
	}
	for _, e := range rows {
		if e.Denom != "ibc/AAA" || !strings.Contains(e.Detail, ReviewUnclassifiedContractMovement) {
			t.Fatalf("flattened row: %s", summary(rows))
		}
	}
}
