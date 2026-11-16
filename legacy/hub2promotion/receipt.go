package hub2promotion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"sort"
)

// The staging receipt is written by the staging importer after it has read
// every sealed part again and compared each stored row with what the sealed
// files give. It is the link between the seals and the exported rows: the
// seals say which recordings are genuine, the receipt says which rows came
// from those recordings, and the finalizer checks that the rows it was handed
// are those rows.
var stagingReceiptSchemas = map[string]string{"cosmoshub-1": "hub1-staging-receipt-v1", "cosmoshub-2": "hub2-staging-receipt-v1"}

// splitTables are the per-height tables the receipt hashes part by part.
var splitTables = []string{"blocks", "tx_inclusions", "block_events", "taxable_events"}

type RowHash struct {
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}
type receiptPart struct {
	First     int64              `json:"first"`
	Last      int64              `json:"last"`
	SealSHA   string             `json:"seal_sha256"`
	Rederived bool               `json:"stored_rows_equal_rows_rederived_from_sealed_source"`
	Tables    map[string]RowHash `json:"tables"`
}
type stagingReceipt struct {
	Schema           string `json:"schema"`
	Chain            string `json:"chain"`
	First            int64  `json:"first"`
	Last             int64  `json:"last"`
	CanonicalThrough int64  `json:"canonical_through"`
	CanonicalHeight  int64  `json:"canonical_height"`
	Complete         bool   `json:"complete_chain"`
	Genesis          struct {
		SHA256 string             `json:"sha256"`
		Equal  bool               `json:"stored_rows_equal_genesis_file"`
		Tables map[string]RowHash `json:"tables"`
	} `json:"genesis"`
	Components []receiptPart `json:"components"`
	Payout     *struct {
		Height    int64   `json:"height"`
		ExportSHA string  `json:"export_records_sha256"`
		Rederived bool    `json:"stored_rows_equal_rows_rederived_from_export"`
		Phase4    RowHash `json:"taxable_events_phase4"`
		ZeroIndex bool    `json:"every_row_tx_index_0_msg_index_0"`
	} `json:"restart_payout"`
	Tail *struct {
		First     int64              `json:"first"`
		Last      int64              `json:"last"`
		SealSHA   string             `json:"tail_seal_sha256"`
		Rederived bool               `json:"stored_rows_equal_rows_rederived_from_captures"`
		TaxRows   *int64             `json:"tax_rows"`
		Tables    map[string]RowHash `json:"tables"`
	} `json:"historical_tail"`
	Projection  map[string]RowHash `json:"projection_tables"`
	Fingerprint string             `json:"fingerprint_all_tables_sha256"`
	Unbound     *int64             `json:"rows_bound_to_no_listed_part"`
}

func readStagingReceipt(p Pin) (stagingReceipt, error) {
	var r stagingReceipt
	b, e := readPinnedBytes(p, 16<<20)
	if e != nil {
		return r, refuse("staging receipt", "missing or changed since it was pinned")
	}
	if _, e = decodeUnique(b); e != nil || json.Unmarshal(b, &r) != nil {
		return r, refuse("staging receipt", "malformed")
	}
	return r, nil
}

// receiptCoversTables checks that the ten exported table files are exactly
// the rows the receipt bound to the sealed parts.
func receiptCoversTables(r stagingReceipt, chain string, tables []Projection) error {
	if want := stagingReceiptSchemas[chain]; want == "" || r.Schema != want || r.Chain != chain {
		return refuse("staging receipt", "not a staging receipt for %s", chain)
	}
	if len(r.Projection) != len(inserts) || len(tables) != len(inserts) {
		return refuse("staging receipt", "does not state all ten loader tables")
	}
	for _, t := range tables {
		got, ok := r.Projection[t.Table]
		if !ok || !shaRE.MatchString(got.SHA256) || got.Rows != t.Rows || got.SHA256 != t.File.SHA256 {
			return refuse("staging receipt", "table %s: the exported rows are not the rows the receipt bound to the sealed parts (%d exported, %d in the receipt)", t.Table, t.Rows, got.Rows)
		}
	}
	return nil
}

// partRows hashes the rows of the per-height tables part by part while the
// projection is streamed, so each part's rows can be compared with what the
// receipt says that sealed part contributed.
type partRows struct {
	parts   []SealedComponent
	buckets map[string][]partBucket
}
type partBucket struct {
	rows int64
	hash hash.Hash
}

func newPartRows(parts []SealedComponent) *partRows {
	p := &partRows{parts: parts, buckets: map[string][]partBucket{}}
	for _, t := range splitTables {
		b := make([]partBucket, len(parts))
		for n := range b {
			b[n].hash = sha256.New()
		}
		p.buckets[t] = b
	}
	return p
}
func (p *partRows) add(table string, height int64, raw []byte) bool {
	n := sort.Search(len(p.parts), func(i int) bool { return p.parts[i].Last >= height })
	if n == len(p.parts) || height < p.parts[n].First {
		return false
	}
	b := &p.buckets[table][n]
	b.rows++
	b.hash.Write(raw)
	b.hash.Write([]byte("\n"))
	return true
}
func (p *partRows) sum(table string, n int) RowHash {
	b := p.buckets[table][n]
	return RowHash{b.rows, hex.EncodeToString(b.hash.Sum(nil))}
}

// checkStagingReceipt makes the seals, the staging database and the export
// one chain of hashes. It refuses unless the receipt lists exactly the seals
// being finalized, in order, and every exported row is one the receipt bound
// to one of those seals, to the restart payout export or to the tail.
func checkStagingReceipt(pin Pin, spec FinalizeSpec, parts []SealedComponent, tables []Projection, facts projectionFacts, b finalBounds) (stagingReceipt, error) {
	const name = "staging receipt"
	r, e := readStagingReceipt(pin)
	if e != nil {
		return r, e
	}
	if e = receiptCoversTables(r, spec.Chain, tables); e != nil {
		return r, e
	}
	if r.First != 1 || r.Last != b.last || r.CanonicalHeight != b.canonical || r.CanonicalThrough != b.canonical || !r.Complete {
		return r, refuse(name, "does not cover the complete chain from height 1 to %d with the migration height %d", b.last, b.canonical)
	}
	if r.Unbound == nil || *r.Unbound != 0 {
		return r, refuse(name, "some stored rows were not bound to any sealed part")
	}
	if r.Genesis.SHA256 != spec.Genesis.SHA256 || !r.Genesis.Equal {
		return r, refuse(name, "staging was built from a different genesis file")
	}
	for _, t := range tables {
		// genesis_files is hashed without its load time there, so only the
		// whole-table entry above can cover it.
		if g, ok := r.Genesis.Tables[t.Table]; ok && t.Table != "genesis_files" && (g.Rows != t.Rows || g.SHA256 != t.File.SHA256) {
			return r, refuse(name, "table %s: genesis rows differ from the exported rows", t.Table)
		}
	}
	var sealed []int
	tail := -1
	for n, c := range parts {
		if !c.Tail {
			sealed = append(sealed, n)
		} else if tail >= 0 {
			return r, refuse(name, "more than one tail part listed")
		} else {
			tail = n
		}
	}
	if len(r.Components) != len(sealed) {
		return r, refuse(name, "lists %d sealed parts, %d are being finalized", len(r.Components), len(sealed))
	}
	for i, n := range sealed {
		c, got := parts[n], r.Components[i]
		if got.SealSHA != c.Acceptance.SHA256 || got.First != c.First || got.Last != c.Last {
			return r, refuse(name, "part %d (heights %d to %d) was staged under a different seal or range than the one being finalized", i+1, c.First, c.Last)
		}
		if !got.Rederived {
			return r, refuse(name, "part %d (heights %d to %d): stored rows were not checked against the sealed recordings", i+1, c.First, c.Last)
		}
		for _, t := range splitTables {
			want, ok := got.Tables[t]
			if have := facts.parts.sum(t, n); !ok || want != have {
				return r, refuse(name, "part %d (heights %d to %d) table %s: exported rows differ from the rows that sealed part contributed (%d exported, %d in the receipt)", i+1, c.First, c.Last, t, have.Rows, want.Rows)
			}
		}
	}
	p := r.Payout
	if p == nil || p.Height != b.canonical || p.ExportSHA != spec.PayoutRecords.SHA256 || !p.Rederived || !p.ZeroIndex {
		return r, refuse(name, "restart payout rows are not bound to the payout records being finalized")
	}
	if p.Phase4.Rows != facts.payoutRows || p.Phase4.SHA256 != facts.payoutSHA {
		return r, refuse(name, "exported restart payout rows differ from the rows derived from the payout records (%d exported, %d in the receipt)", facts.payoutRows, p.Phase4.Rows)
	}
	// Only reduced test boundaries can end at the migration height; a real
	// chain always has its tail.
	if t := r.Tail; b.last == b.canonical {
		if t != nil || tail >= 0 {
			return r, refuse(name, "a tail is listed for a history that ends at the migration height")
		}
	} else {
		if t == nil || tail < 0 || t.First != b.canonical+1 || t.Last != b.last || t.SealSHA != parts[tail].Acceptance.SHA256 || !t.Rederived || t.TaxRows == nil || *t.TaxRows != 0 {
			return r, refuse(name, "tail rows are not bound to the tail seal being finalized")
		}
		for _, table := range splitTables[:3] {
			want, ok := t.Tables[table]
			if have := facts.parts.sum(table, tail); !ok || want != have {
				return r, refuse(name, "tail table %s: exported rows differ from the rows the tail captures gave (%d exported, %d in the receipt)", table, have.Rows, want.Rows)
			}
		}
	}
	if !shaRE.MatchString(r.Fingerprint) {
		return r, refuse(name, "whole-database fingerprint missing")
	}
	return r, nil
}
