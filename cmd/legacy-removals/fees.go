package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// sequence returns the sequence number an account had after height.
func (a *archive) sequence(ctx context.Context, label string, account sdk.AccAddress, height int64) (uint64, error) {
	record, err := a.state(ctx, label, "acc", append([]byte{0x01}, account...), height)
	if err != nil {
		return 0, err
	}
	if len(record) == 0 {
		return 0, fmt.Errorf("the payer has no account record")
	}
	return legacy.StoredAccountSequence(record, account)
}

// feeCounts says what a block did to one account's sequence number as far as
// the block itself shows: how many transactions the account signed whose
// ante handler provably finished, and which transactions it signed whose fee
// is still unknown. The ante handler raises the sequence of every signer of
// a transaction it accepts and of none it refuses.
func feeCounts(items []core.LegacyInclusion, account sdk.AccAddress) (int, []int, error) {
	settled, open := 0, []int(nil)
	for i, item := range items {
		if item.Tx == nil || !item.Tx.SignersComplete {
			return 0, nil, fmt.Errorf("a transaction of the block does not show its signers")
		}
		signs := false
		for _, signer := range item.Tx.Signers {
			signs = signs || bytes.Equal(signer, account)
		}
		switch {
		case !signs:
		case item.Fee.Charge == "charged":
			settled++
		case item.Fee.Charge == "unknown":
			open = append(open, i)
		case item.Fee.Charge != "not charged":
			return 0, nil, fmt.Errorf("a transaction of the block has a fee charge this tool does not know")
		}
	}
	return settled, open, nil
}

// feeVerdict decides the open failures of one account in one block from its
// sequence number after the block before and after the block. The sequence
// moved once for each settled transaction and once for each open failure
// that paid. The open ones are decided only when none paid or all did.
func feeVerdict(before, after uint64, settled, open int) (string, error) {
	if open < 1 {
		return "", fmt.Errorf("the block holds no open failure of the payer")
	}
	if after < before || after-before < uint64(settled) || after-before > uint64(settled+open) {
		return "", fmt.Errorf("the payer's sequence moved by a count the block cannot explain")
	}
	switch after - before - uint64(settled) {
	case 0:
		return "not charged", nil
	case uint64(open):
		return "charged", nil
	}
	return "", fmt.Errorf("the block holds several open failures of the payer and the sequence does not say which paid")
}

// feeStates writes a fee verdict for every listed failed transaction whose
// fee is still unknown after the sequence check's own verdicts. A line is
// written only when two archives served both heights and agree.
func feeStates(ctx context.Context, archives []*archive, root, chain, wanted, resolutions, out string, maxBody int64) {
	verdicts := map[string]string{}
	if resolutions != "" {
		text, err := os.ReadFile(resolutions)
		if err != nil {
			fail("verdict file unavailable")
		}
		for _, line := range strings.Split(strings.TrimSpace(string(text)), "\n") {
			key, verdict, ok := strings.Cut(line, " ")
			if !ok || verdicts[key] != "" {
				fail("the sequence check's verdict file has a repeated line or a line with no verdict")
			}
			verdicts[key] = verdict
		}
	}
	body, err := os.ReadFile(wanted)
	if err != nil {
		fail("the file of transactions is unavailable")
	}
	type place struct{ height, tx int64 }
	seen := map[place]bool{}
	var list []place
	for _, word := range strings.Fields(string(body)) {
		parts := strings.Split(word, "/")
		if len(parts) != 3 || parts[0] != chain {
			fail("a transaction is not chain/height/transaction of this chain")
		}
		var p place
		for i, into := range []*int64{&p.height, &p.tx} {
			if *into, err = strconv.ParseInt(parts[i+1], 10, 64); err != nil || *into < 0 {
				fail("a transaction is not chain/height/transaction of this chain")
			}
		}
		if p.height < 2 {
			fail("a transaction has no block before it")
		}
		if !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	sort.Slice(list, func(a, b int) bool {
		if list[a].height != list[b].height {
			return list[a].height < list[b].height
		}
		return list[a].tx < list[b].tx
	})
	counts := map[string]int{}
	var lines []string
	for _, p := range list {
		h, err := capture.LoadHeight(root, chain, p.height, maxBody)
		if err != nil {
			fail(fmt.Sprintf("height %d: capture not readable", p.height))
		}
		items, err := core.DecodeLegacyBlock(h.Block, h.Results)
		if err == nil {
			err = core.ResolveLegacyFees(items, verdicts)
		}
		if err != nil || p.tx >= int64(len(items)) {
			fail(fmt.Sprintf("height %d: capture not decodable or the transaction is not in it", p.height))
		}
		item := items[p.tx]
		if item.Fee.Charge != "unknown" || len(item.Fee.Payer) == 0 {
			fail(fmt.Sprintf("height %d: the transaction is not a failure with a payer whose fee is still unknown", p.height))
		}
		settled, open, err := feeCounts(items, item.Fee.Payer)
		if err != nil {
			fail(fmt.Sprintf("height %d: %s", p.height, err.Error()))
		}
		label := fmt.Sprintf("fee-%d-%d", p.height, p.tx)
		var answers []string
		for _, a := range archives {
			before, err := a.sequence(ctx, label+"-sequence-before", item.Fee.Payer, p.height-1)
			var after uint64
			if err == nil {
				after, err = a.sequence(ctx, label+"-sequence-after", item.Fee.Payer, p.height)
			}
			if err == errNotServed {
				counts["archive "+a.name+" did not serve the height"]++
				continue
			}
			var answer string
			if err == nil {
				answer, err = feeVerdict(before, after, settled, len(open))
			}
			if err != nil {
				fail(fmt.Sprintf("height %d: archive %s: %s", p.height, a.name, err.Error()))
			}
			answers = append(answers, answer)
		}
		if len(answers) < 2 {
			counts["no line: fewer than two archives served the height"]++
			continue
		}
		if answers[0] != answers[1] {
			fail(fmt.Sprintf("height %d: the two archives disagree", p.height))
		}
		counts[answers[0]+", two archives"]++
		lines = append(lines, fmt.Sprintf("%s/%d/%d %s, two archives", chain, p.height, p.tx, answers[0]))
	}
	text := []byte(strings.Join(lines, "\n") + "\n")
	if len(lines) == 0 {
		text = nil
	}
	if err := os.WriteFile(out, text, 0o600); err != nil {
		fail("verdict file could not be written")
	}
	fmt.Printf("transactions %d, verdict lines %d, verdict file sha256 %x\n", len(list), len(lines), sha256.Sum256(text))
	for _, a := range archives {
		fmt.Printf("archive %s requests %d\n", a.name, a.requests)
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-55s %d\n", k, counts[k])
	}
}
