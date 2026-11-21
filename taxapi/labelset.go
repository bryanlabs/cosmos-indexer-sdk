package taxapi

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LabelSetRow is one address label of a versioned label set. It carries the
// AddressLabel columns plus how the label is known: the rule that made it,
// whether it is derived from chain data or published by its owner (a bridge's
// docs, an exchange's proof of reserves), the query or document it rests on,
// the source's license and the date it was read.
type LabelSetRow struct {
	SetVersion string    `json:"set_version" gorm:"primaryKey"`
	Chain      string    `json:"chain" gorm:"primaryKey"`
	Address    string    `json:"address" gorm:"primaryKey"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	Evidence   string    `json:"evidence"`
	Rule       string    `json:"rule"`
	Basis      string    `json:"basis"`
	Ref        string    `json:"ref"`
	License    string    `json:"license"`
	AsOf       string    `json:"as_of"`
	Tags       []string  `json:"tags" gorm:"serializer:json;type:jsonb"`
	CreatedAt  time.Time `json:"created_at"`
	FileSHA256 string    `json:"-"`
}

// TableName keeps label sets apart from tax_address_labels, which the
// commission tracer reads whole: a set reaches traces only once a version is
// promoted into that table with stop classes for its kinds.
func (LabelSetRow) TableName() string { return "tax_address_label_sets" }

// Label set bases.
const (
	LabelBasisDerived   = "derived"
	LabelBasisPublished = "published"
)

// labelSetHRP is the address prefix each label set chain must use.
var labelSetHRP = map[string]string{"mainnet": "cosmos", "osmosis-1": "osmo", "neutron-1": "neutron"}

// ReadLabelSet parses a JSONL label set and checks every row: one set version,
// a known basis, the provenance fields filled, valid addresses for the chain
// and no address twice. It returns the rows and the file's sha256.
func ReadLabelSet(r io.Reader) ([]LabelSetRow, string, error) {
	h := sha256.New()
	sc := bufio.NewScanner(io.TeeReader(r, h))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var rows []LabelSetRow
	seen := map[[2]string]bool{}
	version := ""
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row LabelSetRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, "", fmt.Errorf("line %d: %w", n, err)
		}
		if err := checkLabelSetRow(row); err != nil {
			return nil, "", fmt.Errorf("line %d: %w", n, err)
		}
		if version == "" {
			version = row.SetVersion
		} else if row.SetVersion != version {
			return nil, "", fmt.Errorf("line %d: set version %q differs from %q", n, row.SetVersion, version)
		}
		k := [2]string{row.Chain, row.Address}
		if seen[k] {
			return nil, "", fmt.Errorf("line %d: address listed twice on %s", n, row.Chain)
		}
		seen[k] = true
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", errors.New("empty label set")
	}
	sum := hex.EncodeToString(h.Sum(nil))
	for i := range rows {
		rows[i].FileSHA256 = sum
	}
	return rows, sum, nil
}

func checkLabelSetRow(row LabelSetRow) error {
	switch {
	case row.SetVersion == "", row.Chain == "", row.Address == "", row.Name == "", row.Kind == "":
		return errors.New("set_version, chain, address, name and kind are required")
	case row.Rule == "", row.Source == "", row.Evidence == "", row.Ref == "", row.AsOf == "":
		return errors.New("rule, source, evidence, ref and as_of are required")
	case row.Basis != LabelBasisDerived && row.Basis != LabelBasisPublished:
		return fmt.Errorf("basis %q is neither derived nor published", row.Basis)
	}
	if hrp, ok := labelSetHRP[row.Chain]; ok {
		got, _, err := bech32.DecodeAndConvert(row.Address)
		if err != nil || got != hrp {
			return fmt.Errorf("address is not a valid %s address", hrp)
		}
	}
	return nil
}

// LabelSetSchema creates the two label set tables. It is plain CREATE
// statements, never ALTER TABLE (the statement summary DDL guard watches ALTER
// TABLE), touches no existing table and grants nothing: the API role cannot
// read these tables, so nothing served changes.
const LabelSetSchema = `CREATE TABLE IF NOT EXISTS tax_address_label_set_versions (
 set_version text PRIMARY KEY,
 file_sha256 text NOT NULL,
 row_count bigint NOT NULL,
 loaded_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS tax_address_label_sets (
 set_version text NOT NULL REFERENCES tax_address_label_set_versions (set_version),
 chain text NOT NULL,
 address text NOT NULL,
 name text NOT NULL,
 kind text NOT NULL,
 source text NOT NULL,
 evidence text NOT NULL,
 rule text NOT NULL,
 basis text NOT NULL CHECK (basis IN ('derived', 'published')),
 ref text NOT NULL,
 license text NOT NULL,
 as_of text NOT NULL,
 tags jsonb,
 created_at timestamptz NOT NULL,
 file_sha256 text NOT NULL,
 PRIMARY KEY (set_version, chain, address)
)`

// LoadLabelSet stores a label set under its version in one transaction. It
// only adds rows: a version already recorded is refused, and no other table is
// written. The version row records the file digest and row count.
func LoadLabelSet(db *gorm.DB, rows []LabelSetRow) (int64, error) {
	if len(rows) == 0 {
		return 0, errors.New("empty label set")
	}
	var stored int64
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(LabelSetSchema).Error; err != nil {
			return err
		}
		res := tx.Exec("INSERT INTO tax_address_label_set_versions (set_version, file_sha256, row_count) VALUES (?, ?, ?) ON CONFLICT DO NOTHING",
			rows[0].SetVersion, rows[0].FileSHA256, len(rows))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("label set %s is already stored", rows[0].SetVersion)
		}
		res = tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != int64(len(rows)) {
			return fmt.Errorf("label set %s: stored %d of %d rows", rows[0].SetVersion, res.RowsAffected, len(rows))
		}
		stored = res.RowsAffected
		return nil
	})
	return stored, err
}
