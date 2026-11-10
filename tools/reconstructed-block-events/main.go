// reconstructed-block-events stores a block's rebuilt BeginBlock events (see
// core/reconstructed_block_events.go) in an already indexed database, the
// same rows a reindex of the block would now write. Without --apply it only
// reports the plan. With --apply it first writes the block's existing event
// rows to a new backup file, then inserts and verifies in one transaction.
//
//	DB_HOST=... DB_NAME=... DB_USER=... DB_PASS=... reconstructed-block-events --height 16596000 [--apply --backup FILE]
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"gorm.io/gorm"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/core"
	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
)

type eventRow struct {
	ID        uint   `json:"id"`
	Lifecycle int    `json:"lifecycle_position"`
	Index     uint64 `json:"index"`
	Type      string `json:"type"`
	AttrIndex *int64 `json:"attr_index"`
	Key       string `json:"key"`
	Value     string `json:"value"`
}

type backup struct {
	ChainID string       `json:"chain_id"`
	Block   models.Block `json:"block"`
	Rows    []eventRow   `json:"rows"`
}

// event is one stored or planned event with its attributes in order.
type event struct {
	Type  string
	Attrs [][2]string
}

func main() { os.Exit(run()) }

func run() int {
	chainID := flag.String("chain-id", "cosmoshub-4", "chain ID")
	height := flag.Int64("height", 0, "block height")
	apply := flag.Bool("apply", false, "insert the rebuilt events (requires --backup)")
	path := flag.String("backup", "", "new backup file, never overwritten")
	flag.Parse()
	if *apply && *path == "" {
		fatal("--apply requires --backup")
	}
	rebuilt, err := core.ReconstructedBeginBlockEvents(*chainID, *height)
	if err != nil {
		fatal(err.Error())
	}
	if rebuilt == nil {
		fatal(fmt.Sprintf("no rebuilt events for %s %d", *chainID, *height))
	}
	want := make([]event, len(rebuilt.BeginBlockEvents))
	types := map[string]int{}
	attrs := 0
	for i, e := range rebuilt.BeginBlockEvents {
		want[i].Type = e.Type
		for _, a := range e.Attributes {
			want[i].Attrs = append(want[i].Attrs, [2]string{a.Key, a.Value})
		}
		types[e.Type]++
		attrs += len(e.Attributes)
	}

	db, err := dbpkg.PostgresDbConnect(env("DB_HOST"), envDefault("DB_PORT", "5432"), env("DB_NAME"), env("DB_USER"), os.Getenv("DB_PASS"), "silent")
	if err != nil {
		fatal(err.Error())
	}
	sqlDB, err := db.DB()
	if err != nil {
		fatal(err.Error())
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if err := db.Exec("SET statement_timeout='300s'; SET lock_timeout='5s'").Error; err != nil {
		fatal(err.Error())
	}

	var chain models.Chain
	if err := db.Where("chain_id = ?", *chainID).First(&chain).Error; err != nil {
		fatal("chain: " + err.Error())
	}
	var block models.Block
	if err := db.Preload("ProposerConsAddress").Preload("Chain").Where("chain_id = ? AND height = ?", chain.ID, *height).First(&block).Error; err != nil {
		fatal("block: " + err.Error())
	}
	rows, err := blockRows(db, block.ID)
	if err != nil {
		fatal(err.Error())
	}
	var first uint64
	if n := rebuilt.EmittedBeginBlockEvents; n >= 0 {
		first = uint64(n)
	} else {
		fatal("negative emitted event count")
	}
	stored := storedFrom(rows, first)
	var failed int64
	if err := db.Table("failed_event_blocks").Where("height = ? AND blockchain_id = ?", *height, chain.ID).Count(&failed).Error; err != nil {
		fatal(err.Error())
	}

	keys := make([]string, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("chain=%s height=%d block_id=%d existing_rows=%d failed_event_blocks=%d\n", *chainID, *height, block.ID, len(rows), failed)
	fmt.Printf("rebuilt events=%d attributes=%d begin_indexes=%d..%d\n", len(want), attrs, first, first+uint64(len(want))-1)
	for _, k := range keys {
		fmt.Printf("  %s=%d\n", k, types[k])
	}
	switch {
	case len(stored) == 0:
	case equal(stored, want):
		fmt.Println("already stored, nothing to do")
		return 0
	default:
		fatal(fmt.Sprintf("%d BeginBlock events already stored at index %d or later and they differ from the rebuilt events", len(stored), first))
	}
	if !*apply {
		fmt.Println("dry run, nothing written")
		return 0
	}

	sum, err := writeBackup(*path, backup{ChainID: *chainID, Block: block, Rows: rows})
	if err != nil {
		fatal("backup: " + err.Error())
	}
	fmt.Printf("backup=%s sha256=%s rows=%d\n", *path, sum, len(rows))

	wrapper := &dbpkg.BlockDBWrapper{
		Block:                         &block,
		UniqueBlockEventTypes:         map[string]models.BlockEventType{},
		UniqueBlockEventAttributeKeys: map[string]models.BlockEventAttributeKey{},
	}
	var conf config.IndexConfig
	if err := core.AppendReconstructedBlockEvents(conf, *chainID, wrapper, rebuilt.EmittedBeginBlockEvents); err != nil {
		fatal(err.Error())
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if _, err := dbpkg.IndexBlockEvents(tx, false, wrapper, "reconstructed"); err != nil {
			return err
		}
		after, err := blockRows(tx, block.ID)
		if err != nil {
			return err
		}
		if !equal(storedFrom(after, first), want) {
			return fmt.Errorf("stored events differ from the rebuilt events")
		}
		if !equal(storedTo(after, first), storedTo(rows, first)) {
			return fmt.Errorf("rows before index %d changed", first)
		}
		return nil
	})
	if err != nil {
		fatal("apply rolled back: " + err.Error())
	}
	fmt.Printf("stored %d events, verified\n", len(want))
	return 0
}

func blockRows(db *gorm.DB, blockID uint) ([]eventRow, error) {
	var rows []eventRow
	err := db.Raw(`SELECT be.id, be.lifecycle_position AS lifecycle, be.index, bet.type, a.index AS attr_index, k.key, a.value
		FROM block_events be
		JOIN block_event_types bet ON bet.id = be.block_event_type_id
		LEFT JOIN block_event_attributes a ON a.block_event_id = be.id
		LEFT JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id
		WHERE be.block_id = ?
		ORDER BY be.lifecycle_position, be.index, a.index`, blockID).Scan(&rows).Error
	return rows, err
}

func collect(rows []eventRow, keep func(eventRow) bool) []event {
	var out []event
	var last *eventRow
	for i := range rows {
		r := rows[i]
		if !keep(r) {
			continue
		}
		if last == nil || last.ID != r.ID {
			out = append(out, event{Type: r.Type})
			last = &rows[i]
		}
		if r.AttrIndex != nil {
			out[len(out)-1].Attrs = append(out[len(out)-1].Attrs, [2]string{r.Key, r.Value})
		}
	}
	return out
}

func storedFrom(rows []eventRow, first uint64) []event {
	return collect(rows, func(r eventRow) bool {
		return r.Lifecycle == int(models.BeginBlockEvent) && r.Index >= first
	})
}

func storedTo(rows []eventRow, first uint64) []event {
	return collect(rows, func(r eventRow) bool {
		return r.Lifecycle != int(models.BeginBlockEvent) || r.Index < first
	})
}

func equal(a, b []event) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// writeBackup writes a new file, syncs it and reads it back before any write.
func writeBackup(path string, b backup) (string, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	check := make([]byte, len(data))
	if _, err := f.ReadAt(check, 0); err != nil {
		return "", err
	}
	if !bytes.Equal(check, data) {
		return "", fmt.Errorf("readback mismatch")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func env(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fatal("missing " + k)
	}
	return v
}

func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func fatal(s string) { fmt.Fprintln(os.Stderr, "reconstructed-block-events:", s); os.Exit(1) }
