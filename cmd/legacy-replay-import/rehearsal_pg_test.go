package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/legacy/ledger"
	rr "github.com/DefiantLabs/cosmos-indexer/legacy/replayrecords"
	"github.com/DefiantLabs/cosmos-indexer/legacy/schema/pgtest"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	amino "github.com/tendermint/go-amino"
	"gorm.io/driver/postgres"
)

type replayTestMsg interface{ synthetic() }
type replayTestSend struct {
	From, To []byte
	Amount   []replayTestCoin
}

func (replayTestSend) synthetic() {}

type replayTestCoin struct{ Denom, Amount string }
type replayTestFee struct {
	Amount []replayTestCoin
	Gas    uint64
}
type replayTestSig struct{ PubKey, Signature []byte }
type replayTestTx struct {
	Msgs       []replayTestMsg
	Fee        replayTestFee
	Signatures []replayTestSig
	Memo       string
}

func TestRehearsalPGActualCommandPathAndNoResume(t *testing.T) {
	db := pgtest.OpenRetained(t)
	var name string
	require.NoError(t, db.Owner.Raw("SELECT current_database()").Scan(&name).Error)
	t.Setenv("LEGACY_REPLAY_IMPORT_DSN", db.Importer.Dialector.(*postgres.Dialector).Config.DSN)
	publicBefore := pgtest.Snapshot(t, db.Owner, "public")
	// Retain synthetic captures and receipts. No TempDir cleanup and no DROP.
	root, e := os.MkdirTemp("", "hub2-importer-command-")
	require.NoError(t, e)
	dir := filepath.Join(root, "cosmoshub-2", "0")
	require.NoError(t, os.MkdirAll(dir, 0700))
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	codec := amino.NewCodec()
	codec.RegisterInterface((*replayTestMsg)(nil), nil)
	codec.RegisterConcrete(replayTestSend{}, "cosmos-sdk/MsgSend", nil)
	codec.RegisterConcrete(replayTestTx{}, "auth/StdTx", nil)
	raw, e := codec.MarshalBinaryLengthPrefixed(replayTestTx{Msgs: []replayTestMsg{replayTestSend{a, b, []replayTestCoin{{"uatom", "53"}}}}, Fee: replayTestFee{Amount: []replayTestCoin{{"uatom", "7"}}, Gas: 90000}})
	require.NoError(t, e)
	gz := func(path, text string) {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		require.NoError(t, e)
		z := gzip.NewWriter(f)
		_, e = z.Write([]byte(text))
		require.NoError(t, e)
		require.NoError(t, z.Close())
		require.NoError(t, f.Close())
	}
	gz(filepath.Join(dir, "9-block.raw.gz"), fmt.Sprintf(`{"result":{"block_meta":{"block_id":{"hash":"%s"}},"block":{"header":{"chain_id":"cosmoshub-2","height":"9","time":"2020-01-01T00:00:00Z","proposer_address":"synthetic"},"data":{"txs":["%s"]}}}}`, strings.Repeat("AB", 32), base64.StdEncoding.EncodeToString(raw)))
	gz(filepath.Join(dir, "9-block_results.raw.gz"), `{"result":{"height":"9","results":{"DeliverTx":[{"code":0}]}}}`)
	records := []rr.Record{
		{Kind: "accounting", Phase: "begin_block", Tx: -1, Message: -1},
		{Kind: "movement", Phase: "ante", Tx: 0, Message: -1, Label: "ante_fee", Address: sdk.AccAddress(a).String(), Denom: "uatom", Amount: "-7"},
		{Kind: "accounting", Phase: "ante", Tx: 0, Message: -1},
		{Kind: "movement", Phase: "message", Tx: 0, Message: 0, Label: "bank/send/bank.send", Address: sdk.AccAddress(a).String(), Denom: "uatom", Amount: "-53"},
		{Kind: "movement", Phase: "message", Tx: 0, Message: 0, Label: "bank/send/bank.send", Address: sdk.AccAddress(b).String(), Denom: "uatom", Amount: "53"},
		{Kind: "accounting", Phase: "message", Tx: 0, Message: 0, Label: "bank/send"},
		{Kind: "accounting", Phase: "transaction", Tx: 0, Message: -1},
		{Kind: "accounting", Phase: "end_block", Tx: -1, Message: -1},
		{Kind: "height_completed", Phase: "block", Tx: -1, Message: -1},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range records {
		r.Height = 9
		r.Status = "committed"
		require.NoError(t, enc.Encode(r))
	}
	digest := sha256.Sum256(buf.Bytes())
	proof := rr.Proof{Chain: "cosmoshub-2", Release: rr.Release034, From: 9, Through: 9, VerifiedThrough: 9, SHA256: hex.EncodeToString(digest[:])}
	write := func(path string, body []byte) {
		f, e := os.OpenFile(filepath.Join(root, path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		require.NoError(t, e)
		_, e = f.Write(body)
		require.NoError(t, e)
		require.NoError(t, f.Close())
	}
	write("records.jsonl", buf.Bytes())
	p, e := json.Marshal(proof)
	require.NoError(t, e)
	write("proof.json", p)
	s, e := json.Marshal(replayStatus{Chain: "cosmoshub-2", Release: rr.Release034, From: 9, Committed: 10, Verified: 9})
	require.NoError(t, e)
	write("status.json", s)
	args := []string{"legacy-replay-import", "--cache", root, "--records", filepath.Join(root, "records.jsonl"), "--proof", filepath.Join(root, "proof.json"), "--status", filepath.Join(root, "status.json"), "--rehearsal-db", name, "--write"}
	invoke := func() error {
		oldFlags, oldArgs := flag.CommandLine, os.Args
		defer func() { flag.CommandLine, os.Args = oldFlags, oldArgs }()
		flag.CommandLine = flag.NewFlagSet("synthetic-rehearsal", flag.ContinueOnError)
		flag.CommandLine.SetOutput(io.Discard)
		os.Args = args
		return run()
	}
	require.NoError(t, invoke())
	conn, e := db.Importer.DB()
	require.NoError(t, e)
	stored, e := ledger.Load(context.Background(), conn, "cosmoshub-2", []int64{9})
	require.NoError(t, e)
	require.Len(t, stored, 1)
	require.Len(t, stored[0].Rows, 1)
	require.Len(t, stored[0].Inclusions, 1)
	require.Equal(t, "pinned_replay_receipt", stored[0].Events[1].Type)
	legacyBefore := pgtest.Snapshot(t, db.Owner, "legacy")
	require.Error(t, invoke(), "populated rehearsal must not resume")
	pgtest.RequireUntouched(t, legacyBefore, pgtest.Snapshot(t, db.Owner, "legacy"))
	pgtest.RequireUntouched(t, publicBefore, pgtest.Snapshot(t, db.Owner, "public"))
}
