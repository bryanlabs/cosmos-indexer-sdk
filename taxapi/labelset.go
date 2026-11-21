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

// LoadLabelSet stores a label set under its version. It only adds rows: a
// version that already has rows is refused, and no other table is written.
func LoadLabelSet(db *gorm.DB, rows []LabelSetRow) (int64, error) {
	if len(rows) == 0 {
		return 0, errors.New("empty label set")
	}
	if err := db.AutoMigrate(&LabelSetRow{}); err != nil {
		return 0, err
	}
	var have int64
	if err := db.Model(&LabelSetRow{}).Where("set_version = ?", rows[0].SetVersion).Count(&have).Error; err != nil {
		return 0, err
	}
	if have > 0 {
		return 0, fmt.Errorf("label set %s already has %d rows", rows[0].SetVersion, have)
	}
	res := db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500)
	return res.RowsAffected, res.Error
}
