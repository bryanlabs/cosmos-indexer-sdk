package taxapi

// Source corrections belong only to a pinned private commission generation.
// They replace no database rows and do not make a whole-accounting claim.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
)

const commissionCorrectionSchema = "commission-native-source-corrections-v1"
const correctionFiveAuthority = "75d80fe8b880c46e53f8c40a1fceceaddd5eb1810b704e43a122deb2a97ec8c7"
const correctionSevenAuthority = "6f197daab25cf15cbc3f687a95a1a3f114204ba7e13dbbfe926d94935dee3fbe"
const correctionSixAuthority = "517d989962e93b47d8e7a4ad115f5047d814cba9701b9d3a6e36370ecf0bcdcb"

// The six tokenize-share reward corrections stood in for message 4 of two
// hub4 txs that an earlier indexer build skipped as undecodable. Once those
// heights are re-indexed the indexer stores the same rewards itself, so this
// manifest, otherwise identical to the six-reward one, carries none. Only these
// exact bytes may omit them; earlier manifests still require all six, so rows
// stored under them stay loadable.
var correctionRewardFreeManifests = map[string]bool{"40d6961721383adb2aa8bf4ad19ac9b8df7f834a3687dd1cd63a144bef654eb4": true}

// This is a derived manifest. Its exact bytes, reviewed normalizer and retained
// authorities are pinned before launch, separately from the unchanged authz profile.
type CommissionSourceCorrections struct {
	Schema               string                             `json:"schema"`
	Chain                string                             `json:"chain"`
	From                 time.Time                          `json:"from"`
	AsOf                 time.Time                          `json:"as_of"`
	BaseProfileSHA256    string                             `json:"base_profile_sha256"`
	FiveAuthoritySHA256  string                             `json:"five_authority_sha256"`
	SevenAuthoritySHA256 string                             `json:"seven_authority_sha256"`
	SixAuthoritySHA256   string                             `json:"six_authority_sha256"`
	NormalizerSHA256     string                             `json:"normalizer_sha256"`
	Windows              []CommissionCorrectionWindow       `json:"windows"`
	Fees                 []CommissionNativeFeeCorrection    `json:"fees"`
	Rewards              []CommissionNativeRewardCorrection `json:"rewards"`
	digest               string
	feesByWallet         map[string][]int
	rewardsByWallet      map[string][]int
}

type CommissionCorrectionWindow struct {
	ID                string    `json:"id"`
	Wallet            string    `json:"wallet"`
	Start             time.Time `json:"start"`
	End               time.Time `json:"end"`
	SourceInputSHA256 string    `json:"source_input_sha256"`
}

type CommissionCorrectionIdentity struct {
	ID          string    `json:"id"`
	TxHash      string    `json:"tx_hash"`
	Height      int64     `json:"height"`
	Timestamp   time.Time `json:"timestamp"`
	Code        int64     `json:"code"`
	ProofSHA256 []string  `json:"proof_sha256"`
	Windows     []string  `json:"windows"`
}

type CommissionNativeFeeCorrection struct {
	CommissionCorrectionIdentity
	ChargedActor  string            `json:"charged_actor"`
	IndexedActor  string            `json:"indexed_actor"`
	SourceNominal map[string]string `json:"source_nominal"`
	Gross         map[string]string `json:"gross"`
	Refund        map[string]string `json:"refund"`
	Net           map[string]string `json:"net"`
	Authorities   []string          `json:"authorities"`
	// The exact source events, not a matching amount or aggregate cash balance,
	// establish that adding this fee does not also debit an indexed BANK leg.
	ChargedActorEventsSHA256  string `json:"charged_actor_events_sha256"`
	SourceBANKFeeDebitOverlap *bool  `json:"source_BANK_fee_debit_overlap"`
}

type CommissionNativeRewardCorrection struct {
	CommissionCorrectionIdentity
	Recipient                   string `json:"recipient"`
	Denom                       string `json:"denom"`
	Amount                      string `json:"amount"`
	NativeMessageIndex          int    `json:"native_message_index"`
	NativeEventIndex            int    `json:"native_event_index"`
	MessageType                 string `json:"message_type"`
	OriginalMessageEventsSHA256 string `json:"original_message_events_sha256"`
	SourceBANKCreditOverlap     *bool  `json:"source_BANK_credit_overlap"`
}

// commissionCorrectionRefusal names the correction check that refused a read.
// Its text is a fixed sentence with no address, amount or driver text, so a
// private read failure can keep it as its cause.
type commissionCorrectionRefusal string

func correctionRefusal(text string) error { return commissionCorrectionRefusal(text) }

func (e commissionCorrectionRefusal) Error() string { return string(e) }

// check is the refusal as a cause slug, such as
// correction_reward_source_footprint_changed.
func (e commissionCorrectionRefusal) check() string {
	words := strings.FieldsFunc(strings.TrimPrefix(string(e), "native "), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	return strings.ToLower(strings.Join(words, "_"))
}

// correctionEmptyFootprint is the fingerprint of a message with no rows for the
// wallet: what the reward corrections were proved against.
var correctionEmptyFootprint = privateSHA([]byte("[]"))

func correctionHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func correctionInteger(s string) (*big.Int, error) {
	if s == "" || len(s) > 128 {
		return nil, fmt.Errorf("invalid native correction integer")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return nil, fmt.Errorf("invalid native correction integer")
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return nil, fmt.Errorf("noncanonical native correction integer")
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("invalid native correction integer")
	}
	return v, nil
}
func correctionCoins(m map[string]string) (map[string]*big.Int, error) {
	if m == nil || len(m) > 64 {
		return nil, correctionRefusal("native correction coin vector missing")
	}
	out := map[string]*big.Int{}
	for d, s := range m {
		if d == "" || len(d) > 256 {
			return nil, fmt.Errorf("invalid native correction denomination")
		}
		v, e := correctionInteger(s)
		if e != nil {
			return nil, e
		}
		if v.Sign() != 0 {
			out[d] = v
		}
	}
	return out, nil
}
func correctionCoinEqual(a, b map[string]string) bool {
	x, e := correctionCoins(a)
	if e != nil {
		return false
	}
	y, e := correctionCoins(b)
	if e != nil || len(x) != len(y) {
		return false
	}
	for d, v := range x {
		if y[d] == nil || v.Cmp(y[d]) != 0 {
			return false
		}
	}
	return true
}
func (p *CommissionSourceCorrections) validate() error { return p.validateRewards(6) }

func (p *CommissionSourceCorrections) validateRewards(rewards int) error {
	if p.Schema != commissionCorrectionSchema || p.Chain != "cosmoshub-4" || !p.From.Equal(flowDay(p.From)) || !p.AsOf.Equal(flowDay(p.AsOf)) || !p.From.Before(p.AsOf) || !correctionHash(p.BaseProfileSHA256) || !correctionHash(p.NormalizerSHA256) || p.FiveAuthoritySHA256 != correctionFiveAuthority || p.SevenAuthoritySHA256 != correctionSevenAuthority || p.SixAuthoritySHA256 != correctionSixAuthority {
		return correctionRefusal("native correction manifest authority or window mismatch")
	}
	if len(p.Windows) != 6 || len(p.Fees) > 786 || len(p.Fees) < 779 || len(p.Rewards) != rewards {
		return correctionRefusal("native correction bounded scope mismatch")
	}
	windows := map[string]CommissionCorrectionWindow{}
	for _, w := range p.Windows {
		if w.ID == "" || w.Wallet == "" || !w.Start.Before(w.End) || !correctionHash(w.SourceInputSHA256) {
			return correctionRefusal("native correction source window invalid")
		}
		if _, ok := windows[w.ID]; ok {
			return fmt.Errorf("duplicate native correction window")
		}
		windows[w.ID] = w
	}
	ids := map[string]bool{}
	txs := map[string]bool{}
	authorities := map[string]map[string]bool{correctionFiveAuthority: {}, correctionSevenAuthority: {}}
	check := func(c CommissionCorrectionIdentity, actor string) error {
		if !correctionHash(c.ID) || !correctionHash(strings.ToLower(c.TxHash)) || c.Height <= 0 || c.Timestamp.IsZero() || c.Timestamp.Before(p.From) || !c.Timestamp.Before(p.AsOf) || actor == "" || len(c.ProofSHA256) == 0 || len(c.Windows) == 0 {
			return correctionRefusal("native correction identity invalid")
		}
		if ids[c.ID] {
			return fmt.Errorf("duplicate native correction identity")
		}
		ids[c.ID] = true
		for _, h := range c.ProofSHA256 {
			if !correctionHash(h) {
				return correctionRefusal("native correction proof digest invalid")
			}
		}
		seen := map[string]bool{}
		for _, id := range c.Windows {
			w, ok := windows[id]
			if !ok || seen[id] || w.Wallet != actor || c.Timestamp.Before(w.Start) || !c.Timestamp.Before(w.End) {
				return correctionRefusal("native correction source membership mismatch")
			}
			seen[id] = true
		}
		return nil
	}
	p.feesByWallet = map[string][]int{}
	p.rewardsByWallet = map[string][]int{}
	for i, c := range p.Fees {
		if e := check(c.CommissionCorrectionIdentity, c.ChargedActor); e != nil {
			return e
		}
		if txs[c.TxHash] || c.IndexedActor == "" || !correctionHash(c.ChargedActorEventsSHA256) || (c.SourceBANKFeeDebitOverlap == nil || *c.SourceBANKFeeDebitOverlap) || len(c.Authorities) == 0 {
			return fmt.Errorf("native fee source binding invalid")
		}
		txs[c.TxHash] = true
		gross, e := correctionCoins(c.Gross)
		if e != nil {
			return e
		}
		refund, e := correctionCoins(c.Refund)
		if e != nil {
			return e
		}
		net, e := correctionCoins(c.Net)
		if e != nil {
			return e
		}
		if _, e = correctionCoins(c.SourceNominal); e != nil {
			return e
		}
		all := map[string]bool{}
		for d := range gross {
			all[d] = true
		}
		for d := range refund {
			all[d] = true
		}
		for d := range net {
			all[d] = true
		}
		for d := range all {
			g, r, n := new(big.Int), new(big.Int), new(big.Int)
			if gross[d] != nil {
				g.Set(gross[d])
			}
			if refund[d] != nil {
				r.Set(refund[d])
			}
			if net[d] != nil {
				n.Set(net[d])
			}
			if g.Sub(g, r).Cmp(n) != 0 {
				return fmt.Errorf("native fee charged/refund/net mismatch")
			}
		}
		seen := map[string]bool{}
		for _, a := range c.Authorities {
			if authorities[a] == nil || seen[a] {
				return fmt.Errorf("native fee authority invalid")
			}
			seen[a] = true
			authorities[a][c.TxHash] = true
		}
		p.feesByWallet[c.ChargedActor] = append(p.feesByWallet[c.ChargedActor], i)
		if c.IndexedActor != c.ChargedActor {
			p.feesByWallet[c.IndexedActor] = append(p.feesByWallet[c.IndexedActor], i)
		}
	}
	if len(authorities[correctionFiveAuthority]) != 779 || len(authorities[correctionSevenAuthority]) != 7 {
		return fmt.Errorf("native fee union authority coverage mismatch")
	}
	nativeRewards := map[string]bool{}
	for i, c := range p.Rewards {
		if e := check(c.CommissionCorrectionIdentity, c.Recipient); e != nil {
			return e
		}
		if c.Code != 0 || c.Denom != apiUatom || c.NativeMessageIndex < 0 || c.NativeEventIndex < 0 || c.MessageType == "" || !correctionHash(c.OriginalMessageEventsSHA256) || (c.SourceBANKCreditOverlap == nil || *c.SourceBANKCreditOverlap) {
			return fmt.Errorf("native reward source binding invalid")
		}
		n, e := correctionInteger(c.Amount)
		if e != nil || n.Sign() <= 0 {
			return fmt.Errorf("native reward amount invalid")
		}
		k := fmt.Sprintf("%s/%d/%d/%s/%s", c.TxHash, c.NativeMessageIndex, c.NativeEventIndex, c.Recipient, c.Denom)
		if nativeRewards[k] {
			return fmt.Errorf("duplicate native reward event")
		}
		nativeRewards[k] = true
		p.rewardsByWallet[c.Recipient] = append(p.rewardsByWallet[c.Recipient], i)
	}
	return nil
}

func LoadCommissionSourceCorrections(path, expected string) (*CommissionSourceCorrections, error) {
	original, e := os.Lstat(path)
	if e != nil || !original.Mode().IsRegular() {
		return nil, correctionRefusal("native correction manifest must be a regular non-symlink file")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !os.SameFile(original, st) || !st.Mode().IsRegular() || st.Size() > 64<<20 {
		return nil, correctionRefusal("native correction manifest must be a bounded regular file")
	}
	b, e := io.ReadAll(io.LimitReader(f, 64<<20+1))
	if e != nil {
		return nil, e
	}
	if len(b) > 64<<20 || !correctionHash(expected) || privateSHA(b) != expected {
		return nil, correctionRefusal("native correction manifest digest mismatch")
	}
	var p CommissionSourceCorrections
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil {
		return nil, correctionRefusal("native correction manifest decode failed")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, correctionRefusal("native correction manifest trailing data")
	}
	rewards := 6
	if correctionRewardFreeManifests[expected] {
		rewards = 0
	}
	if e = p.validateRewards(rewards); e != nil {
		return nil, e
	}
	p.digest = expected
	return &p, nil
}

type CommissionSourceCorrectionProfileEvidence struct {
	ProfileSHA256        string
	BaseProfileSHA256    string
	FiveAuthoritySHA256  string
	SevenAuthoritySHA256 string
	SixAuthoritySHA256   string
	NormalizerSHA256     string
}

// A compact receipt covers each actual wallet read, including a genuine empty
// intersection. It is never a statement that unvisited wallets are unaffected.
type CommissionSourceCorrectionReceipt struct {
	ProfileSHA256 string
	WalletSHA256  string
	Start         time.Time
	End           time.Time
	Snapshot      string
	Status        string
	Effects       []CommissionSourceEffectReceipt `json:",omitempty"`
}
type CommissionSourceEffectReceipt struct {
	EffectID                string
	TxHashSHA256            string
	ProofSHA256             []string
	Kind                    string
	Disposition             string
	SourceTxID              uint
	SourceMessageID         uint     `json:",omitempty"`
	OriginalActorFeePresent bool     `json:",omitempty"`
	ResolvedSourceKey       *flowKey `json:",omitempty"`
	NativeEventIndex        int      `json:",omitempty"`
	SourceRowsSHA256        string   `json:",omitempty"`
}
type commissionCorrectionJournal struct {
	mu   sync.Mutex
	rows map[string]CommissionSourceCorrectionReceipt
}

func newCorrectionJournal() *commissionCorrectionJournal {
	return &commissionCorrectionJournal{rows: map[string]CommissionSourceCorrectionReceipt{}}
}
func (j *commissionCorrectionJournal) retain(r CommissionSourceCorrectionReceipt) {
	b, _ := json.Marshal(r)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.rows[privateSHA(b)] = r
}
func (j *commissionCorrectionJournal) merge(other *commissionCorrectionJournal) {
	for _, r := range other.receipts() {
		j.retain(r)
	}
}
func (j *commissionCorrectionJournal) receipts() []CommissionSourceCorrectionReceipt {
	j.mu.Lock()
	defer j.mu.Unlock()
	keys := make([]string, 0, len(j.rows))
	for k := range j.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]CommissionSourceCorrectionReceipt, 0, len(keys))
	for _, k := range keys {
		out = append(out, j.rows[k])
	}
	return out
}

// Source event fingerprints use a deliberately small, stable projection, not
// labels, pricing, source row timestamps or a manufactured taxable-event ID.
type commissionCorrectionEvent struct {
	MessageID uint      `json:"message_id"`
	SubIndex  int       `json:"sub_index"`
	Category  string    `json:"category"`
	Amount    string    `json:"amount"`
	Denom     string    `json:"denom"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Validator string    `json:"validator"`
	Trigger   string    `json:"trigger"`
	Detail    string    `json:"detail"`
	Height    int64     `json:"height"`
	Timestamp time.Time `json:"timestamp"`
	TxHash    string    `json:"tx_hash"`
}

func correctionEventsSHA(events []tax.TaxableEvent, hash string, message uint) string {
	selected := make([]commissionCorrectionEvent, 0)
	for _, e := range events {
		// The withdraw-address moves came after the profiles were recorded
		// and change no flow, so they stay out of the fingerprint.
		if e.TxHash != hash || (message != 0 && e.MessageID != message) || isWithdrawAddressMove(e) {
			continue
		}
		selected = append(selected, commissionCorrectionEvent{e.MessageID, e.SubIndex, e.Category, e.Amount, e.Denom, e.FromAddr, e.ToAddr, e.ValidatorAddress, e.RewardTrigger, e.Detail, e.BlockHeight, e.Timestamp.UTC(), e.TxHash})
	}
	sort.Slice(selected, func(i, j int) bool {
		a, _ := json.Marshal(selected[i])
		b, _ := json.Marshal(selected[j])
		return bytes.Compare(a, b) < 0
	})
	b, _ := json.Marshal(selected)
	return privateSHA(b)
}

// Every lookup is performed inside the existing private read-only snapshot.
// No native/RPC call, label mutation or financial source write is available here.
type commissionCorrectionSourceTx struct {
	ID        uint
	Hash      string
	Height    int64
	Timestamp time.Time
	Code      int64
}

// Match the existing archive contract: indexed PostgreSQL times have only
// microsecond precision, while retained native headers have nanoseconds.
func correctionTimestampEqual(native, indexed time.Time) bool {
	return native.Sub(indexed).Abs() < time.Microsecond
}

func (s *Server) correctionSourceTx(chain string, c CommissionCorrectionIdentity) (commissionCorrectionSourceTx, error) {
	var rows []commissionCorrectionSourceTx
	e := s.db.Raw(`SELECT x.id,x.hash,b.height,b.time_stamp AS timestamp,x.code FROM txes x JOIN blocks b ON b.id=x.block_id JOIN chains c ON c.id=b.chain_id WHERE x.hash=? AND c.chain_id IN ?`, c.TxHash, chainIDs(chain)).Scan(&rows).Error
	if e != nil {
		return commissionCorrectionSourceTx{}, e
	}
	if len(rows) != 1 || rows[0].ID == 0 || rows[0].Hash != c.TxHash || rows[0].Height != c.Height || !correctionTimestampEqual(c.Timestamp, rows[0].Timestamp) || rows[0].Code != c.Code {
		return commissionCorrectionSourceTx{}, correctionRefusal("native correction indexed transaction identity changed")
	}
	return rows[0], nil
}
func (s *Server) validateCorrectionFeeSource(tx uint, actor string, c CommissionNativeFeeCorrection) (bool, error) {
	var rows []struct{ Actor, Denom, Amount string }
	if e := s.db.Raw(`SELECT a.address AS actor,d.base AS denom,f.amount::text AS amount FROM fees f JOIN addresses a ON a.id=f.payer_address_id JOIN denoms d ON d.id=f.denomination_id WHERE f.tx_id=? ORDER BY f.id`, tx).Scan(&rows).Error; e != nil {
		return false, e
	}
	coins := map[string]string{}
	actorFeePresent := false
	for _, r := range rows {
		if r.Actor != c.IndexedActor && r.Actor != c.ChargedActor {
			return false, correctionRefusal("native correction fee payer source changed")
		}
		actorFeePresent = actorFeePresent || r.Actor == actor
		v, e := correctionInteger(r.Amount)
		if e != nil {
			return false, e
		}
		prior := new(big.Int)
		if coins[r.Denom] != "" {
			prior.SetString(coins[r.Denom], 10)
		}
		coins[r.Denom] = prior.Add(prior, v).String()
	}
	if len(rows) > 0 && !correctionCoinEqual(coins, c.SourceNominal) {
		return false, correctionRefusal("native correction nominal source fee changed")
	}
	var grants int64
	if e := s.db.Table("tax_fee_grants").Where("tx_id=?", tx).Count(&grants).Error; e != nil {
		return false, e
	}
	if grants != 0 {
		return false, correctionRefusal("native correction fee grant not covered by retained proof")
	}
	return actorFeePresent, nil
}
func (s *Server) correctionAnteMessage(tx uint) (uint, error) {
	var rows []struct{ ID uint }
	if err := s.db.Raw(`SELECT m.id FROM messages m WHERE m.tx_id=? AND m.message_index=0`, tx).Scan(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) != 1 || rows[0].ID == 0 {
		return 0, correctionRefusal("native correction ante message identity is unavailable")
	}
	return rows[0].ID, nil
}

func (s *Server) correctionMessage(tx uint, c CommissionNativeRewardCorrection) (uint, error) {
	var rows []struct {
		ID          uint
		MessageType string
	}
	if e := s.db.Raw(`SELECT m.id,mt.message_type FROM messages m JOIN message_types mt ON mt.id=m.message_type_id WHERE m.tx_id=? AND m.message_index=?`, tx, c.NativeMessageIndex).Scan(&rows).Error; e != nil {
		return 0, e
	}
	if len(rows) != 1 || rows[0].ID == 0 || rows[0].MessageType != c.MessageType {
		return 0, correctionRefusal("native correction indexed message identity changed")
	}
	return rows[0].ID, nil
}

// Defer known fees until source identity and no-overlap are checked after all
// wallet rows load. This also avoids requiring a missing failed-fee ledger row
// when the retained native proof already supplies the exact charged/refund pair.
func (s *Server) deferNativeCorrectionFee(chain, addr, hash string, at, start, end time.Time) (bool, error) {
	p := s.commissionSourceCorrections
	if !s.readOnlyLedger || p == nil || chainParam(chain) != apiMainnet {
		return false, nil
	}
	for _, i := range p.feesByWallet[addr] {
		c := p.Fees[i]
		if c.TxHash != hash {
			continue
		}
		if !correctionTimestampEqual(c.Timestamp, at) || c.Timestamp.Before(start) || !c.Timestamp.Before(end) {
			return false, correctionRefusal("native correction indexed fee time or period changed")
		}
		return true, nil
	}
	return false, nil
}

// Lifecycle rows lack an indexed message phase. They can include EndBlock
// releases, so their position relative to an intra-block correction is unknown.
func correctionUnrankedNativeBlocks(events []tax.TaxableEvent, addr string) map[int64]bool {
	blocks := map[int64]bool{}
	for _, event := range events {
		if event.MessageID != 0 {
			continue
		}
		if flow, ok := flowEventFromRow(event, addr, apiUatom); ok && flow.Amount.IsPositive() {
			blocks[event.BlockHeight] = true
		}
	}
	return blocks
}

func (s *Server) applyCommissionSourceCorrections(chain, addr string, start, end time.Time, rows walletRows) (walletRows, error) {
	p := s.commissionSourceCorrections
	if p == nil {
		return rows, nil
	}
	if !s.readOnlyLedger || s.readSnapshotStarted.IsZero() || s.sourceCorrectionReceipts == nil || s.privateProjectionSnapshot == "" || chainParam(chain) != apiMainnet || !correctionHash(p.digest) {
		return walletRows{}, correctionRefusal("native correction requires exact private snapshot")
	}
	r := CommissionSourceCorrectionReceipt{ProfileSHA256: p.digest, WalletSHA256: privateSHA([]byte(addr)), Start: start.UTC(), End: end.UTC(), Snapshot: s.privateProjectionSnapshot, Status: "not_applicable_to_this_observed_wallet_period"}
	// Copy only slices modified below. Source rows and another reader's input
	// remain byte-for-byte unchanged, including on a failed correction.
	out := rows
	out.Fees = append([]feeRec(nil), rows.Fees...)
	out.CommissionCorrections = append([]flowEvent(nil), rows.CommissionCorrections...)
	out.IndexedRewardCorrections = maps.Clone(rows.IndexedRewardCorrections)
	unrankedBlocks := correctionUnrankedNativeBlocks(rows.Events, addr)
	for _, i := range p.feesByWallet[addr] {
		c := p.Fees[i]
		if c.Timestamp.Before(start) || !c.Timestamp.Before(end) {
			continue
		}
		source, e := s.correctionSourceTx(chain, c.CommissionCorrectionIdentity)
		if e != nil {
			return walletRows{}, e
		}
		if source.Timestamp.Before(start) || !source.Timestamp.Before(end) {
			return walletRows{}, correctionRefusal("native correction indexed transaction crossed read boundary")
		}
		actorFeePresent, e := s.validateCorrectionFeeSource(source.ID, addr, c)
		if e != nil {
			return walletRows{}, e
		}
		anteMessage, e := s.correctionAnteMessage(source.ID)
		if e != nil {
			return walletRows{}, e
		}
		fingerprint := correctionEventsSHA(rows.Events, c.TxHash, 0)
		if addr == c.ChargedActor && fingerprint != c.ChargedActorEventsSHA256 {
			return walletRows{}, correctionRefusal("native correction source BANK/events footprint changed")
		}
		for _, f := range out.Fees {
			if f.Hash == c.TxHash {
				return walletRows{}, correctionRefusal("native correction original fee not deferred")
			}
		}
		resolvedKey := flowKey{Time: source.Timestamp.UTC(), Height: c.Height, Message: anteMessage, Sub: -1}
		disposition := "no_wrong_indexed_payer_fee_present"
		if actorFeePresent {
			disposition = "removed_wrong_indexed_payer_fee"
		}
		if addr == c.ChargedActor {
			disposition = "native_charged_minus_refund_applied"
			denoms := make([]string, 0, len(c.Net))
			for d := range c.Net {
				denoms = append(denoms, d)
			}
			sort.Strings(denoms)
			for _, d := range denoms {
				v, _ := correctionInteger(c.Net[d])
				if v.Sign() != 0 {
					if d == apiUatom && unrankedBlocks[c.Height] {
						return walletRows{}, correctionRefusal("native correction same-block lifecycle ordering is not proved")
					}
					out.Fees = append(out.Fees, feeRec{Hash: c.TxHash, Ts: source.Timestamp.UTC(), Denom: d, Amount: v.String(), commissionKey: &resolvedKey})
				}
			}
		}
		r.Effects = append(r.Effects, CommissionSourceEffectReceipt{EffectID: c.ID, TxHashSHA256: privateSHA([]byte(c.TxHash)), ProofSHA256: append([]string(nil), c.ProofSHA256...), Kind: "fee", Disposition: disposition, SourceTxID: source.ID, SourceMessageID: anteMessage, OriginalActorFeePresent: actorFeePresent, ResolvedSourceKey: &resolvedKey, SourceRowsSHA256: fingerprint})
	}
	// Retained reward events are independently identified by native outer-message
	// and event indices. Their real indexed message ID is resolved, never invented.
	// Existing same-message credits commute for both origin rules; reject a debit
	// or commission origin in that message rather than inventing source ordering.
	// A message the indexer now stores itself is used as indexed only when its
	// rows for the wallet are exactly the corrected rewards; the corrections
	// are then left out, so the coins are never counted twice.
	type rewardGroup struct {
		message     uint
		originalSHA string
		sourceSHA   string
		indexed     bool
		nextSub     int
		effects     []CommissionNativeRewardCorrection
		tx          commissionCorrectionSourceTx
	}
	groups := map[string]*rewardGroup{}
	order := []string{}
	for _, i := range p.rewardsByWallet[addr] {
		c := p.Rewards[i]
		if c.Timestamp.Before(start) || !c.Timestamp.Before(end) {
			continue
		}
		source, e := s.correctionSourceTx(chain, c.CommissionCorrectionIdentity)
		if e != nil {
			return walletRows{}, e
		}
		if source.Timestamp.Before(start) || !source.Timestamp.Before(end) {
			return walletRows{}, correctionRefusal("native correction indexed reward crossed read boundary")
		}
		message, e := s.correctionMessage(source.ID, c)
		if e != nil {
			return walletRows{}, e
		}
		if unrankedBlocks[c.Height] {
			return walletRows{}, correctionRefusal("native correction same-block lifecycle ordering is not proved")
		}
		key := fmt.Sprintf("%s/%d", c.TxHash, message)
		g := groups[key]
		if g == nil {
			fingerprint := correctionEventsSHA(rows.Events, c.TxHash, message)
			indexed := fingerprint != c.OriginalMessageEventsSHA256
			if indexed && c.OriginalMessageEventsSHA256 != correctionEmptyFootprint {
				return walletRows{}, correctionRefusal("native correction reward source footprint changed")
			}
			if indexed && !correctionIndexedRewardsEqual(rows.Events, addr, source, message, correctionMessageRewards(p, addr, c, start, end)) {
				return walletRows{}, correctionRefusal("native correction reward source footprint changed")
			}
			g = &rewardGroup{message: message, originalSHA: c.OriginalMessageEventsSHA256, sourceSHA: fingerprint, indexed: indexed, tx: source}
			for _, event := range rows.Events {
				if event.TxHash == c.TxHash && event.MessageID == message {
					flow, ok := flowEventFromRow(event, addr, apiUatom)
					if ok && (flow.Dir != "in" || flow.Category != apiReward) {
						return walletRows{}, correctionRefusal("native correction reward ordering is not proved")
					}
					if event.SubIndex >= g.nextSub {
						g.nextSub = event.SubIndex + 1
					}
				}
			}
			groups[key] = g
			order = append(order, key)
		} else if g.originalSHA != c.OriginalMessageEventsSHA256 {
			return walletRows{}, correctionRefusal("native correction reward source proof conflicts")
		}
		g.effects = append(g.effects, c)
	}
	for _, key := range order {
		g := groups[key]
		sort.Slice(g.effects, func(i, j int) bool { return g.effects[i].NativeEventIndex < g.effects[j].NativeEventIndex })
		if g.indexed {
			total := decimal.Zero
			for _, c := range g.effects {
				amount, e := decimal.NewFromString(c.Amount)
				if e != nil {
					return walletRows{}, e
				}
				total = total.Add(amount)
				r.Effects = append(r.Effects, CommissionSourceEffectReceipt{EffectID: c.ID, TxHashSHA256: privateSHA([]byte(c.TxHash)), ProofSHA256: append([]string(nil), c.ProofSHA256...), Kind: "reward", Disposition: "indexed_reward_equals_correction_not_added", SourceTxID: g.tx.ID, SourceMessageID: g.message, NativeEventIndex: c.NativeEventIndex, SourceRowsSHA256: g.sourceSHA})
			}
			if out.IndexedRewardCorrections == nil {
				out.IndexedRewardCorrections = map[uint]decimal.Decimal{}
			}
			out.IndexedRewardCorrections[g.message] = out.IndexedRewardCorrections[g.message].Add(total)
			continue
		}
		for _, c := range g.effects {
			amount, e := decimal.NewFromString(c.Amount)
			if e != nil {
				return walletRows{}, e
			}
			resolvedKey := flowKey{Time: g.tx.Timestamp.UTC(), Height: c.Height, Message: g.message, Sub: g.nextSub}
			out.CommissionCorrections = append(out.CommissionCorrections, flowEvent{Key: resolvedKey, Category: apiReward, Dir: "in", Amount: amount, To: addr, Trigger: flowSourceCorrectionTrigger})
			g.nextSub++
			r.Effects = append(r.Effects, CommissionSourceEffectReceipt{EffectID: c.ID, TxHashSHA256: privateSHA([]byte(c.TxHash)), ProofSHA256: append([]string(nil), c.ProofSHA256...), Kind: "reward", Disposition: "native_credit_added_after_commuting_message_credits", SourceTxID: g.tx.ID, SourceMessageID: g.message, NativeEventIndex: c.NativeEventIndex, ResolvedSourceKey: &resolvedKey, SourceRowsSHA256: g.originalSHA})
		}
	}
	if len(r.Effects) > 0 {
		r.Status = "bounded_native_effects_consumed"
	}
	s.sourceCorrectionReceipts.retain(r)
	return out, nil
}

// correctionMessageRewards is every reward correction of addr in [start,end)
// for c's transaction and outer message.
func correctionMessageRewards(p *CommissionSourceCorrections, addr string, c CommissionNativeRewardCorrection, start, end time.Time) []CommissionNativeRewardCorrection {
	var out []CommissionNativeRewardCorrection
	for _, i := range p.rewardsByWallet[addr] {
		o := p.Rewards[i]
		if o.TxHash == c.TxHash && o.NativeMessageIndex == c.NativeMessageIndex && !o.Timestamp.Before(start) && o.Timestamp.Before(end) {
			out = append(out, o)
		}
	}
	return out
}

// correctionIndexedRewardsEqual reports whether the wallet's indexed rows in a
// corrected reward message are exactly those rewards: every row an inbound
// uatom reward of that tx, block and time, with each denom's total equal to the
// corrections' total. The indexer books one row per validator while the
// corrections hold one per share record, so totals are compared, not rows.
// Corrections name no validator; each row must name one. Rows are chosen by
// message id alone, so a row of the message under another hash, or a
// withdraw-address move, refuses instead of being left out.
func correctionIndexedRewardsEqual(events []tax.TaxableEvent, addr string, tx commissionCorrectionSourceTx, message uint, effects []CommissionNativeRewardCorrection) bool {
	want := map[string]*big.Int{}
	for _, c := range effects {
		v, err := correctionInteger(c.Amount)
		if err != nil || v.Sign() <= 0 {
			return false
		}
		if want[c.Denom] == nil {
			want[c.Denom] = new(big.Int)
		}
		want[c.Denom].Add(want[c.Denom], v)
	}
	got := map[string]*big.Int{}
	for _, e := range events {
		if e.MessageID != message {
			continue
		}
		if e.TxHash != tx.Hash || isWithdrawAddressMove(e) || e.Category != apiReward || e.ToAddr != addr || e.FromAddr == addr || e.ValidatorAddress == "" || e.RewardTrigger != "" || e.BlockHeight != tx.Height || !correctionTimestampEqual(tx.Timestamp, e.Timestamp) || want[e.Denom] == nil {
			return false
		}
		v, err := correctionInteger(e.Amount)
		if err != nil || v.Sign() <= 0 {
			return false
		}
		if got[e.Denom] == nil {
			got[e.Denom] = new(big.Int)
		}
		got[e.Denom].Add(got[e.Denom], v)
	}
	if len(got) != len(want) {
		return false
	}
	for d, v := range want {
		if got[d] == nil || got[d].Cmp(v) != 0 {
			return false
		}
	}
	return true
}

// indexedRewardCorrectionsKept refuses a trace whose payout reconciliation
// changed a message whose indexed rewards were taken in place of its
// corrections: that message must credit exactly the corrected total, as the
// corrections themselves would.
func indexedRewardCorrectionsKept(addr string, events []flowEvent, proved map[uint]decimal.Decimal) error {
	if len(proved) == 0 {
		return nil
	}
	got := map[uint]decimal.Decimal{}
	for _, e := range events {
		if _, ok := proved[e.Key.Message]; ok && e.Category == apiReward && e.Dir != apiOut && e.To == addr {
			got[e.Key.Message] = got[e.Key.Message].Add(e.Amount)
		}
	}
	for message, want := range proved {
		if !got[message].Equal(want) {
			return correctionRefusal("native correction indexed reward payout differs")
		}
	}
	return nil
}
