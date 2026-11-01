package db

import (
	"reflect"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestBlockIndexWindowQuery(t *testing.T) {
	// DryRun plus disabled ping constructs SQL without contacting a database.
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dryRunPostgresDSN}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	var rows []BlockIndexState
	stmt := blockIndexWindowQuery(db, 7, 31760000, 31769999).Find(&rows).Statement
	want := `SELECT height, tx_indexed, block_events_indexed FROM "blocks" WHERE chain_id = $1::int AND time_stamp != '0001-01-01T00:00:00.000Z' AND height >= $2 AND height <= $3 ORDER BY height asc LIMIT 10000`
	if stmt.SQL.String() != want {
		t.Fatalf("query = %s", stmt.SQL.String())
	}
	if !reflect.DeepEqual(stmt.Vars, []interface{}{uint(7), int64(31760000), int64(31769999)}) {
		t.Fatalf("query arguments = %v", stmt.Vars)
	}
	if strings.Contains(stmt.SQL.String(), "OFFSET") {
		t.Fatal("offset pagination used")
	}
}

func TestBlockIndexWindowRejectsUnboundedRanges(t *testing.T) {
	for _, bounds := range [][2]int64{{0, 1}, {1, -1}, {1, 10001}, {5, 4}} {
		// A nil DB also proves invalid bounds fail before any query.
		if _, err := GetBlockIndexWindow(nil, 1, bounds[0], bounds[1]); err == nil {
			t.Fatalf("accepted invalid bounds %v", bounds)
		}
	}
}
