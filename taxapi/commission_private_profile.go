package taxapi

// This is a separate, source-bound, read-only staging projection. It does not
// erase persisted parser errors or change the ordinary/public readiness gate.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/cosmos/cosmos-sdk/x/authz"
)

const privateParserSourceSHA = "7af52846f68ef38f3e02de6106762b3854b95d72083a600fadcc5b5b53c0fe1d"
const privateEconomicProofSHA = "ed927d64ed94a8675ccebf2f0bc8f0e787e75e5b4be32ecbf86c661ef385aafe"
const privateOriginalMessageSHA = "b22781c798ab832f40ad1b1c7ec33fbbb53128264a3150db78d031b128efa5a3"
const privateOriginalErrorSHA = "cdf5f19bae10e6f42b34f1808d2b2619d645df659fac55755bc3cd1ef9012401"
const privateOriginalTxSHA = "df16775e9770aff4f38e39bdd8b3f0e60319a474c3cea087714f80cc10ccbc10"
const privateSourceSealSHA = "6ad0881533f774728fe0b96dfad6c26d61772786b2e4a42251391860b09a09ee"

// Every original row, xmin, attribute order, fee, signer and database identity
// participates. No snapshot ID is hashed: each read obtains a new repeatable-read
// snapshot, then must reproduce the exact same retained source inside it.
const privateProfileSourceSQL = `SELECT jsonb_build_object(
 'system_id',(SELECT system_identifier::text FROM pg_control_system()),
 'database',current_database(),'database_oid',(SELECT oid::text FROM pg_database WHERE datname=current_database()),
 'chain',to_jsonb(c),'block',to_jsonb(b),'block_xmin',b.xmin::text,
 'tx',to_jsonb(t),'tx_xmin',t.xmin::text,
 'message',to_jsonb(m),'message_bytes',encode(m.message_bytes,'base64'),'message_xmin',m.xmin::text,'message_type',mt.message_type,
 'error',to_jsonb(e),'error_xmin',e.xmin::text,
 'all_errors',(SELECT coalesce(jsonb_agg(jsonb_build_array(to_jsonb(pe),pe.xmin::text) ORDER BY pe.id),'[]'::jsonb) FROM message_parser_errors pe JOIN messages pm ON pm.id=pe.message_id WHERE pm.tx_id=t.id),
 'all_messages',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(mm),mm.xmin::text) ORDER BY mm.id) FROM messages mm WHERE mm.tx_id=t.id),
 'tax_rows',(SELECT coalesce(jsonb_agg(to_jsonb(te) ORDER BY te.id),'[]'::jsonb) FROM taxable_events te WHERE te.message_id=m.id),
 'fees',(SELECT coalesce(jsonb_agg(jsonb_build_array(to_jsonb(f),f.xmin::text,to_jsonb(d),to_jsonb(a)) ORDER BY f.id),'[]'::jsonb) FROM fees f JOIN denoms d ON d.id=f.denomination_id JOIN addresses a ON a.id=f.payer_address_id WHERE f.tx_id=t.id),
 'signers',(SELECT coalesce(jsonb_agg(jsonb_build_array(to_jsonb(s),to_jsonb(a)) ORDER BY s.address_id),'[]'::jsonb) FROM tx_signer_addresses s JOIN addresses a ON a.id=s.address_id WHERE s.tx_id=t.id),
 'events',(SELECT coalesce(jsonb_agg(jsonb_build_object('row',to_jsonb(me),'xmin',me.xmin::text,'type',et.type,'attributes',
 (SELECT coalesce(jsonb_agg(jsonb_build_object('row',to_jsonb(a),'xmin',a.xmin::text,'key',k.key,'value',a.value) ORDER BY a.index,a.id),'[]'::jsonb) FROM message_event_attributes a JOIN message_event_attribute_keys k ON k.id=a.message_event_attribute_key_id WHERE a.message_event_id=me.id)) ORDER BY me.index,me.id),'[]'::jsonb) FROM message_events me JOIN message_event_types et ON et.id=me.message_event_type_id WHERE me.message_id=m.id)
 )::text AS source
 FROM messages m JOIN message_types mt ON mt.id=m.message_type_id JOIN txes t ON t.id=m.tx_id JOIN blocks b ON b.id=t.block_id JOIN chains c ON c.id=b.chain_id JOIN message_parser_errors e ON e.message_id=m.id
 WHERE m.id=27907375 AND e.id=10262 AND t.id=18802568 AND b.height=27991521 AND c.chain_id='cosmoshub-4' AND t.code=0`

type CommissionPrivateProfile struct {
	Schema              string    `json:"schema"`
	SourceSHA256        string    `json:"source_sha256"`
	ParserSHA256        string    `json:"parser_sha256"`
	EconomicProofSHA256 string    `json:"economic_proof_sha256"`
	From                time.Time `json:"from"`
	AsOf                time.Time `json:"as_of"`
	digest              string
}

func privateSHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func LoadCommissionPrivateProfile(path, expectedSHA string) (*CommissionPrivateProfile, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return nil, fmt.Errorf("private profile must be a bounded regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(expectedSHA) != 64 || privateSHA(b) != expectedSHA {
		return nil, fmt.Errorf("private profile digest mismatch")
	}
	var p CommissionPrivateProfile
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return nil, fmt.Errorf("private profile decode failed")
	}
	if p.Schema != "commission-readonly-authz-projection-v1" || p.SourceSHA256 != privateSourceSealSHA || p.ParserSHA256 != privateParserSourceSHA || p.EconomicProofSHA256 != privateEconomicProofSHA || p.From.Format(time.DateOnly) != "2024-03-21" || p.AsOf.Format(time.DateOnly) != "2026-11-13" || !p.From.Equal(flowDay(p.From)) || !p.AsOf.Equal(flowDay(p.AsOf)) {
		return nil, fmt.Errorf("private profile source, parser, economics or window mismatch")
	}
	p.digest = expectedSHA
	return &p, nil
}

// A receipt reports original and projected counts separately. It never replaces
// the retained error with a synthetic successful source row.
type CommissionPrivateProfileEvidence struct {
	ProfileSHA256       string
	SourceSHA256        string
	ParserSHA256        string
	EconomicProofSHA256 string
}

type CommissionPrivateProjectionReceipt struct {
	ProfileSHA256            string
	SourceSHA256             string
	Snapshot                 string
	WalletScopeSHA256        string
	Start                    time.Time
	End                      time.Time
	OriginalParserErrors     int64
	ReclassifiedParserErrors int64
	RemainingParserErrors    int64
}

func (s *Server) validatePrivateProjection() error {
	p := s.commissionPrivateProfile
	if p == nil {
		return nil
	}
	if p.SourceSHA256 != privateSourceSealSHA || p.ParserSHA256 != privateParserSourceSHA || p.EconomicProofSHA256 != privateEconomicProofSHA || len(p.digest) != 64 {
		return fmt.Errorf("private projection profile changed")
	}
	if !s.readOnlyLedger || s.readSnapshotStarted.IsZero() {
		return fmt.Errorf("private projection requires private read snapshot")
	}
	var guard struct {
		RO, Isolation, Snapshot string
		NoXid                   bool
	}
	if err := s.db.Raw("SELECT current_setting('transaction_read_only') AS ro,current_setting('transaction_isolation') AS isolation,pg_current_snapshot()::text AS snapshot,pg_current_xact_id_if_assigned() IS NULL AS no_xid").Scan(&guard).Error; err != nil {
		return err
	}
	if guard.RO != "on" || guard.Isolation != "repeatable read" || !guard.NoXid {
		return fmt.Errorf("private projection snapshot guard failed")
	}
	var rows []struct{ Source string }
	if err := s.db.Raw(privateProfileSourceSQL).Scan(&rows).Error; err != nil {
		return err
	}
	if len(rows) != 1 || privateSHA([]byte(rows[0].Source)) != p.SourceSHA256 {
		return fmt.Errorf("private projection original source changed")
	}
	if err := validatePrivateSourceParse([]byte(rows[0].Source)); err != nil {
		return err
	}
	s.privateProjectionSnapshot = guard.Snapshot
	return nil
}

type commissionProjectionJournal struct {
	mu   sync.Mutex
	rows map[string]CommissionPrivateProjectionReceipt
}

func (j *commissionProjectionJournal) retain(addresses []string, start, end time.Time, r CommissionPrivateProjectionReceipt) {
	scope := append([]string(nil), addresses...)
	sort.Strings(scope)
	b, _ := json.Marshal(scope)
	r.WalletScopeSHA256 = privateSHA(b)
	r.Start = start
	r.End = end
	key := r.WalletScopeSHA256 + start.String() + end.String() + r.Snapshot
	j.mu.Lock()
	defer j.mu.Unlock()
	j.rows[key] = r
}
func (s *Server) privateProjectionTrace(base *dbFlowLoader) (*Server, *dbFlowLoader) {
	r := NewServer(s.db, s.oracle, s.native)
	r.readOnlyLedger = s.readOnlyLedger
	r.privateReadSnapshotTimeout = s.privateReadSnapshotTimeout
	r.commissionPrivateProfile = s.commissionPrivateProfile
	r.privateProjectionReceipts = &commissionProjectionJournal{rows: map[string]CommissionPrivateProjectionReceipt{}}
	l := &dbFlowLoader{s: r, chain: base.chain, node: base.node, private: base.private, archive: base.archive, mu: &sync.Mutex{}, labels: map[string]AddressLabel{}, chans: map[string]string{}}
	base.mu.Lock()
	for k, v := range base.labels {
		l.labels[k] = v
	}
	for k, v := range base.chans {
		l.chans[k] = v
	}
	base.mu.Unlock()
	return r, l
}
func (j *commissionProjectionJournal) merge(pending *commissionProjectionJournal) {
	pending.mu.Lock()
	defer pending.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	for k, v := range pending.rows {
		j.rows[k] = v
	}
}

func (j *commissionProjectionJournal) receipts() []CommissionPrivateProjectionReceipt {
	j.mu.Lock()
	defer j.mu.Unlock()
	keys := make([]string, 0, len(j.rows))
	for k := range j.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]CommissionPrivateProjectionReceipt, 0, len(keys))
	for _, k := range keys {
		out = append(out, j.rows[k])
	}
	return out
}

func validatePrivateSourceParse(raw []byte) error {
	var source struct {
		MessageBytes []byte `json:"message_bytes"`
		MessageType  string `json:"message_type"`
		Error        struct {
			Error string `json:"error"`
		} `json:"error"`
		Tx struct {
			Hash string `json:"hash"`
		} `json:"tx"`
		Events []struct {
			Type       string                        `json:"type"`
			Attributes []struct{ Key, Value string } `json:"attributes"`
		} `json:"events"`
		TaxRows []json.RawMessage `json:"tax_rows"`
	}
	if json.Unmarshal(raw, &source) != nil || source.MessageType != "/cosmos.authz.v1beta1.MsgExec" || privateSHA(source.MessageBytes) != privateOriginalMessageSHA || privateSHA([]byte(source.Error.Error)) != privateOriginalErrorSHA || privateSHA([]byte(source.Tx.Hash)) != privateOriginalTxSHA || len(source.TaxRows) != 0 {
		return fmt.Errorf("private projection original message/error/effects mismatch")
	}
	var m authz.MsgExec
	if m.Unmarshal(source.MessageBytes) != nil || len(m.Msgs) != 1 || m.Msgs[0] == nil || m.Msgs[0].TypeUrl != "/cosmos.staking.v1beta1.MsgEditValidator" {
		return fmt.Errorf("private projection message is not the reviewed metadata case")
	}
	log := &txlog.LogMessage{}
	for _, ev := range source.Events {
		e := txlog.LogMessageEvent{Type: ev.Type}
		for _, a := range ev.Attributes {
			e.Attributes = append(e.Attributes, txlog.Attribute{Key: a.Key, Value: a.Value})
		}
		log.Events = append(log.Events, e)
	}
	data, err := (&tax.Parser{}).ParseMessage(&m, log, config.IndexConfig{})
	if err != nil || data == nil {
		return fmt.Errorf("private projection corrected parse failed")
	}
	effects, ok := (*data).([]tax.TaxableEvent)
	if !ok || len(effects) != 0 {
		return fmt.Errorf("private projection produced economic effects; staging rows required")
	}
	return nil
}
