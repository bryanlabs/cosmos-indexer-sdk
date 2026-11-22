package taxapi

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/tax"
)

func TestInChunksCoversEveryValueInOrder(t *testing.T) {
	for _, n := range []int{0, 1, bindListChunk - 1, bindListChunk, bindListChunk + 1, 3*bindListChunk + 7} {
		values := make([]int, n)
		for i := range values {
			values[i] = i
		}
		var got []int
		calls := 0
		if err := inChunks(values, func(chunk []int) error {
			calls++
			if len(chunk) == 0 || len(chunk) > bindListChunk {
				t.Fatalf("n=%d: chunk of %d", n, len(chunk))
			}
			got = append(got, chunk...)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if want := (n + bindListChunk - 1) / bindListChunk; calls != want {
			t.Fatalf("n=%d: %d calls, want %d", n, calls, want)
		}
		for i, v := range got {
			if v != i {
				t.Fatalf("n=%d: value %d at %d", n, v, i)
			}
		}
		if len(got) != n {
			t.Fatalf("n=%d: got %d values", n, len(got))
		}
	}
	stop := errors.New("stop")
	calls := 0
	err := inChunks(make([]int, 3*bindListChunk), func([]int) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("error %v after %d calls, want stop after 1", err, calls)
	}
}

// A wallet with more distinct non-native transaction hashes than one
// statement may bind still gets every memo. pgx refuses more than 65535 bind
// parameters before the query reaches PostgreSQL.
func TestEventMemosMoreHashesThanBindLimit(t *testing.T) {
	db := isolatedAPIFixtureDB(t)
	if err := dbpkg.MigrateModels(db); err != nil {
		t.Fatal(err)
	}
	const n = 70001
	for _, q := range []string{
		"INSERT INTO addresses(id,address) VALUES(1,'cosmos1proposer')",
		"INSERT INTO chains(id,chain_id) VALUES(1,'cosmoshub-4')",
		"INSERT INTO blocks(id,chain_id,proposer_cons_address_id,height,time_stamp,tx_indexed) VALUES(1,1,1,31197048,'2026-05-20T08:15:44Z',true)",
		fmt.Sprintf(`INSERT INTO txes(id,block_id,hash,code,memo)
SELECT g, 1, 'MEMOTX' || lpad(g::text, 6, '0'), 0, CASE WHEN g %% 7 = 0 THEN '' ELSE 'memo ' || g END
FROM generate_series(1, %d) g`, n),
		fmt.Sprintf("INSERT INTO txes(id,block_id,hash,code,memo) VALUES(%d,1,'NATIVEONLY',0,'native memo'),(%d,1,'UNNAMED',0,'not asked')", n+1, n+2),
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	hash := func(i int) string { return fmt.Sprintf("MEMOTX%06d", i) }
	var events []tax.TaxableEvent
	want := map[string]string{}
	for i := 1; i <= n; i++ {
		events = append(events, tax.TaxableEvent{TxHash: hash(i), Denom: "ibc/VOUCHER"})
		// A transaction with several legs adds no second bind value.
		if i%5 == 0 {
			events = append(events, tax.TaxableEvent{TxHash: hash(i), Denom: "ibc/OTHER"})
		}
		memo := fmt.Sprintf("memo %d", i)
		if i%7 == 0 {
			memo = ""
		}
		want[hash(i)] = memo
	}
	// Native rows are not asked for, and a hash with no indexed
	// transaction has no entry.
	events = append(events,
		tax.TaxableEvent{TxHash: "NATIVEONLY", Denom: apiUatom},
		tax.TaxableEvent{TxHash: "NOTINDEXED", Denom: "ibc/VOUCHER"},
	)

	distinct := make([]string, 0, n+1)
	for i := 1; i <= n; i++ {
		distinct = append(distinct, hash(i))
	}
	distinct = append(distinct, "NOTINDEXED")
	var unchunked []struct{ Hash, Memo string }
	err := db.Table("txes").Select("hash,memo").Where("hash IN ?", distinct).Scan(&unchunked).Error
	if err == nil || !strings.Contains(err.Error(), "65535") {
		t.Fatalf("one IN list of %d hashes: error %v, want the 65535 parameter limit", len(distinct), err)
	}

	s := &Server{db: db}
	got, err := s.eventMemos(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d memos, want %d", len(got), len(want))
	}
	for h, memo := range want {
		if v, ok := got[h]; !ok || v != memo {
			t.Fatalf("memo of %s = %q (present %v), want %q", h, v, ok, memo)
		}
	}
	if _, ok := got["NATIVEONLY"]; ok {
		t.Fatal("native-only transaction memo was read")
	}
	if _, ok := got["NOTINDEXED"]; ok {
		t.Fatal("memo for a transaction that is not indexed")
	}

	// Below the limit the chunked read returns the same map as one IN list.
	small := events[:3*bindListChunk]
	gotSmall, err := s.eventMemos(small)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var hashes []string
	for _, e := range small {
		if e.Denom != apiUatom && !seen[e.TxHash] {
			seen[e.TxHash] = true
			hashes = append(hashes, e.TxHash)
		}
	}
	var rows []struct{ Hash, Memo string }
	if err := db.Table("txes").Select("hash,memo").Where("hash IN ?", hashes).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(gotSmall) {
		t.Fatalf("chunked read %d memos, single read %d", len(gotSmall), len(rows))
	}
	for _, r := range rows {
		if gotSmall[r.Hash] != r.Memo {
			t.Fatalf("memo of %s: chunked %q, single %q", r.Hash, gotSmall[r.Hash], r.Memo)
		}
	}
}
