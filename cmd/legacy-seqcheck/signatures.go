package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/core"
	"github.com/DefiantLabs/cosmos-indexer/legacy"
	"github.com/DefiantLabs/cosmos-indexer/legacy/capture"
	"github.com/DefiantLabs/cosmos-indexer/legacy/sequence"
	bank "github.com/cosmos/cosmos-sdk/x/bank/types"
)

type signedTx struct {
	key, charge, first string
	messages           int
	doc                *legacy.SignDoc
	signers, receivers []string
	signatures         []legacy.Signature
}

type signedHeight struct {
	state string
	fault string
	txs   []signedTx
}

func loadSigned(root, chain string, height, maxBody int64) signedHeight {
	h, err := capture.LoadTransactions(root, chain, height, maxBody)
	if errors.Is(err, os.ErrNotExist) {
		return signedHeight{state: "not in the cache"}
	}
	if err != nil {
		return signedHeight{state: "unreadable", fault: safe(err)}
	}
	if len(h.Block.Block.Txs) == 0 {
		return signedHeight{state: "read"}
	}
	items, err := core.DecodeLegacyBlock(h.Block, h.Results)
	if err != nil {
		return signedHeight{state: "not decoded", fault: safe(err)}
	}
	out := signedHeight{state: "read"}
	for _, item := range items {
		if !item.Tx.SignersComplete {
			return signedHeight{state: "not counted", fault: "transaction signers incomplete"}
		}
		tx := signedTx{key: item.Identity, charge: item.Fee.Charge, signatures: item.Tx.Signatures}
		tx.messages = len(item.Tx.Messages)
		if len(item.Tx.Messages) > 0 {
			tx.first = item.Tx.Messages[0].AminoName
		}
		for _, signer := range item.Tx.Signers {
			tx.signers = append(tx.signers, signer.String())
		}
		tx.doc, _ = item.Tx.SignDoc(chain)
		if item.Result != nil && item.Result.Code == 0 {
			for _, msg := range item.Tx.Messages {
				switch m := msg.Derived.(type) {
				case *bank.MsgSend:
					tx.receivers = append(tx.receivers, m.ToAddress)
				case *bank.MsgMultiSend:
					for _, output := range m.Outputs {
						tx.receivers = append(tx.receivers, output.Address)
					}
				}
			}
		}
		out.txs = append(out.txs, tx)
	}
	return out
}

// followSignatures reads the range in chain order and follows every account's
// sequence through the signatures. Heights are decoded in parallel a batch at
// a time and verified in order.
func followSignatures(root, chain string, from, to, maxBody int64, workers int, every time.Duration, opening, closing *sequence.Genesis) (*sequence.Ledger, map[string]int64, map[string]int64, map[string]int64) {
	ledger := sequence.NewLedger(opening, closing)
	heights, faults, unchecked := map[string]int64{}, map[string]int64{}, map[string]int64{}
	keys := map[string][]byte{}
	if from > 1 {
		ledger.Gap()
	}
	const batch = 4000
	started, lastLine := time.Now(), time.Now()
	var transactions int64
	for start := from; start <= to; start += batch {
		if every > 0 && time.Since(lastLine) >= every {
			seconds := time.Since(started).Seconds()
			fmt.Printf("progress chain=%s step=signatures next=%d of=%d heights_read=%d transactions=%d heights_per_second=%.1f transactions_per_second=%.1f\n",
				chain, start, to, start-from, transactions, float64(start-from)/seconds, float64(transactions)/seconds)
			lastLine = time.Now()
		}
		end := start + batch - 1
		if end > to {
			end = to
		}
		loaded := make([]signedHeight, end-start+1)
		jobs := make(chan int64, batch)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for height := range jobs {
					loaded[height-start] = loadSigned(root, chain, height, maxBody)
				}
			}()
		}
		for height := start; height <= end; height++ {
			jobs <- height
		}
		close(jobs)
		wg.Wait()
		for _, h := range loaded {
			heights[h.state]++
			if h.fault != "" {
				faults[h.fault]++
			}
			if h.state != "read" {
				ledger.Gap()
				continue
			}
			transactions += int64(len(h.txs))
			for _, tx := range h.txs {
				signers := make([]sequence.Signed, len(tx.signers))
				for i, signer := range tx.signers {
					signers[i].Signer = signer
					reason := ""
					var key, signature []byte
					switch {
					case tx.doc == nil:
						reason = "sign bytes not rebuilt, first message " + tx.first
					case len(tx.signatures) != len(tx.signers):
						reason = "signature count differs from signer count"
					default:
						signature = tx.signatures[i].Signature
						key = tx.signatures[i].PublicKeyRaw
						switch {
						case len(key) == 0:
							if key = keys[signer]; key == nil {
								reason = "public key absent and not seen before"
							}
						case tx.signatures[i].Address.String() != signer:
							// The chain checked a signature against the key it had
							// on record for the account and ignored a key written
							// in the transaction once one was recorded.
							if key = keys[signer]; key == nil {
								reason = "public key is not the signer's and none seen before"
							} else {
								unchecked["checked against the recorded key, the one in the transaction is not the signer's"]++
							}
						default:
							keys[signer] = key
						}
					}
					if reason != "" {
						unchecked[reason]++
						continue
					}
					doc := tx.doc
					signers[i].Kind = fmt.Sprintf("%d messages, first %s, key of %d bytes", tx.messages, tx.first, len(key))
					signers[i].Try = func(account, seq uint64) bool {
						return legacy.VerifySignature(key, signature, doc.Bytes(account, seq))
					}
				}
				ledger.Add(tx.key, tx.charge, signers)
				for _, receiver := range tx.receivers {
					ledger.Received(receiver)
				}
			}
		}
	}
	return ledger, heights, faults, unchecked
}
