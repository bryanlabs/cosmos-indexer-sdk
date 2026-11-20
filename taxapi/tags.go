package taxapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// User tags are the user's own classification of a transaction, the way a
// bookkeeper annotates a bank line. They are supplied with each request (the
// web app keeps them per device and per saved report), never stored against
// the on-chain record, and they change only what an exporter emits.
const (
	TagInternalTransfer = "internal_transfer" // between the user's own wallets or an exchange account: not a disposal
	TagPayment          = "payment"           // paid for goods or services (out), or received as payment for them (in)
	TagIncome           = "income"            // received as income other than staking
	TagGift             = "gift"              // given or received as a gift
	TagAirdrop          = "airdrop"           // received as an airdrop
	TagProtocolDeposit  = "protocol_deposit"  // still owned, held by a protocol: not a disposal
	TagExpense          = "expense"           // a business or protocol expense
	TagSpam             = "spam"              // unsolicited dust or a scam token: excluded from tax imports
	TagLost             = "lost"              // lost or stolen
)

// TagVocabulary lists every tag with the treatment it produces.
func TagVocabulary() []map[string]string {
	return []map[string]string{
		{apiTag: TagInternalTransfer, apiLabel: "Internal transfer", apiTreatment: "Not a disposal or acquisition. Sent to tax software as a transfer so it matches the other wallet. Left out of Form 8949."},
		{apiTag: TagPayment, apiLabel: "Payment", apiTreatment: "Sent coins are a disposal at fair market value. Received coins are income at receipt and that value is their cost basis."},
		{apiTag: TagIncome, apiLabel: apiIncomeLabel, apiTreatment: "Ordinary income at the USD value on receipt; that value is the cost basis."},
		{apiTag: TagGift, apiLabel: "Gift", apiTreatment: "A gift sent is not a sale. A gift received carries the giver's basis, which we cannot see, so a later disposal shows basis unknown."},
		{apiTag: TagAirdrop, apiLabel: "Airdrop", apiTreatment: "Ordinary income at the USD value on receipt."},
		{apiTag: TagProtocolDeposit, apiLabel: "Protocol deposit", apiTreatment: "Still owned. Kept out of tax-software imports and Form 8949, shown in the Generic CSV and journal."},
		{apiTag: TagExpense, apiLabel: "Expense", apiTreatment: "A disposal at fair market value, labelled as a cost or expense for tax software."},
		{apiTag: TagSpam, apiLabel: "Spam", apiTreatment: "Excluded from tax-software imports and Form 8949. Kept in the Generic CSV for the audit trail."},
		{apiTag: TagLost, apiLabel: "Lost or stolen", apiTreatment: "Labelled lost for tax software. Whether a loss is deductible depends on jurisdiction."},
	}
}

var validTags = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range TagVocabulary() {
		m[t[apiTag]] = true
	}
	return m
}()

// UserTags maps a key to a tag. A key is one of three forms:
//
//   - "<hash>", every row of the transaction;
//   - "<hash>:<denom>", the rows of one asset within it;
//   - "<hash>#<message_index>#<sub_index>", one row: the message's index in
//     the transaction and the row's index within that message (negative for
//     the lot legs the API derives).
//
// A row key names exactly the rows tagged, so rows a later replay adds to the
// transaction (a withdraw-address move, a contract's supplement row) inherit
// nothing. MigrateUserTags turns the first two forms into row keys.
type UserTags map[string]string

const rowKeySep = "#"

const maxUserTags = 20000

// ParseUserTags validates the JSON object form of UserTags.
func ParseUserTags(input string) (UserTags, error) {
	input = strings.TrimSpace(input)
	if input == "" || input == "{}" {
		return UserTags{}, nil
	}
	if len(input) > 4<<20 {
		return nil, fmt.Errorf("tags too large")
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil, fmt.Errorf("tags must be a JSON object of {\"tx_hash\": \"tag\"}: %w", err)
	}
	if len(raw) > maxUserTags {
		return nil, fmt.Errorf("at most %d tags per request", maxUserTags)
	}
	out := make(UserTags, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(strings.ToLower(v))
		if k == "" || len(k) > 200 {
			return nil, fmt.Errorf("invalid tag key %q", k)
		}
		if v == "" {
			continue
		}
		if !validTags[v] {
			return nil, fmt.Errorf("unknown tag %q for %s", v, k)
		}
		key, err := normalizeTagKey(k)
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}

// normalizeTagKey upper-cases the hash of a key and, for a row key, checks
// both indexes and writes them in canonical form.
func normalizeTagKey(k string) (string, error) {
	if !strings.Contains(k, rowKeySep) {
		return strings.ToUpper(strings.SplitN(k, ":", 2)[0]) + denomSuffix(k), nil
	}
	parts := strings.Split(k, rowKeySep)
	if len(parts) != 3 || parts[0] == "" || strings.Contains(parts[0], ":") {
		return "", fmt.Errorf("row tag key %q must be <tx_hash>#<message_index>#<sub_index>", k)
	}
	msg, err := strconv.Atoi(parts[1])
	if err != nil || msg < 0 {
		return "", fmt.Errorf("row tag key %q has an invalid message index", k)
	}
	sub, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", fmt.Errorf("row tag key %q has an invalid sub index", k)
	}
	return rowTagKey(parts[0], msg, sub), nil
}

func rowTagKey(hash string, messageIndex, subIndex int) string {
	return strings.ToUpper(hash) + rowKeySep + strconv.Itoa(messageIndex) + rowKeySep + strconv.Itoa(subIndex)
}

// rowKey is the row's own tag key, or "" for a row that has none: a fee, a
// block event with no message, or a row whose message index was not loaded.
func (r Row) rowKey() string {
	if r.Category == apiFee || r.MessageID == 0 || r.TxHash == "" || !r.hasMessageIndex {
		return ""
	}
	return rowTagKey(r.TxHash, r.MessageIndex, r.SubIndex)
}

// hasRowKeys reports whether any key is a row key.
func (t UserTags) hasRowKeys() bool {
	for k := range t {
		if strings.Contains(k, rowKeySep) {
			return true
		}
	}
	return false
}

func denomSuffix(key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		return key[i:]
	}
	return ""
}

// Canonical renders the tags sorted, for cache keys.
func (t UserTags) Canonical() string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(t[k])
		b.WriteByte(';')
	}
	return b.String()
}

func tagsFromRequest(r *http.Request) (UserTags, error) {
	if len(r.URL.Query()[apiTags]) > 1 {
		return nil, assetReviewInputError{fmt.Errorf("tags must be supplied once")}
	}
	t, err := ParseUserTags(r.URL.Query().Get(apiTags))
	if err != nil {
		return nil, assetReviewInputError{err}
	}
	return t, nil
}

// ApplyTags stamps UserTag on the rows a tag addresses. A row key wins over a
// denom-specific tag, which wins over a transaction-wide one. Fees are never
// tagged, and neither is a withdraw-address move under any key form: a tag on
// its reward transaction would otherwise turn the move into income or a
// disposal, and the reward stays the delegator's.
func ApplyTags(rows []Row, tags UserTags) []Row {
	if len(tags) == 0 {
		return rows
	}
	for i := range rows {
		if t, _, ok := tagFor(rows[i], tags); ok {
			rows[i].UserTag = t
		}
	}
	return rows
}

// tagFor is the tag ApplyTags gives the row and the key it came from.
func tagFor(r Row, tags UserTags) (tag, key string, ok bool) {
	if r.Category == apiFee || r.isRewardMove() {
		return "", "", false
	}
	hash := strings.ToUpper(r.TxHash)
	for _, k := range []string{r.rowKey(), hash + ":" + r.Denom, hash} {
		if k == "" {
			continue
		}
		if t, found := tags[k]; found {
			return t, k, true
		}
	}
	return "", "", false
}

// TagMigration counts what MigrateUserTags did.
type TagMigration struct {
	RowKeys int `json:"row_keys"` // row keys written
	Dropped int `json:"dropped"`  // transaction and asset keys replaced by row keys
	Kept    int `json:"kept"`     // keys carried over unchanged
}

// MigrateUserTags rewrites transaction and asset tags as row tags over the
// rows of one report. Every row a tag addresses today, except fees and
// withdraw-address moves, gets its own row key with that tag. A transaction
// or asset key is dropped once every row it addressed has its row key, so
// rows a later replay adds to the transaction inherit nothing. Keys that
// addressed no row here, or a row with no row key, are kept, and so are row
// keys already present. ApplyTags gives every one of these rows the same tag
// under the old tags and the migrated ones.
func MigrateUserTags(rows []Row, tags UserTags) (UserTags, TagMigration, error) {
	out := make(UserTags, len(tags))
	used := map[string]bool{}
	pinned := map[string]bool{}
	for k, v := range tags {
		out[k] = v
	}
	var m TagMigration
	for _, r := range rows {
		tag, key, ok := tagFor(r, tags)
		if !ok {
			continue
		}
		rk := r.rowKey()
		switch {
		case rk == "":
			pinned[key] = true
		case key == rk:
		default:
			used[key] = true
			if prior, seen := out[rk]; seen && prior != tag {
				return nil, TagMigration{}, fmt.Errorf("row %s would take tags %q and %q", rk, prior, tag)
			}
			if _, seen := out[rk]; !seen {
				m.RowKeys++
			}
			out[rk] = tag
		}
	}
	for k := range used {
		if !pinned[k] {
			delete(out, k)
			m.Dropped++
		}
	}
	m.Kept = len(out) - m.RowKeys
	return out, m, nil
}

// handleTagMigrate returns a report's tags with its transaction and asset
// keys rewritten as row keys (MigrateUserTags), for the caller to store in
// place of the old ones: GET /tags/migrate?chain=&addresses=&start=&end=&tags=
// It reads the same rows the report does, without asset decisions, which
// never add or remove a row.
func (s *Server) handleTagMigrate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addrParam := q.Get(apiAddresses)
	if addrParam == "" {
		addrParam = q.Get(apiAddress)
	}
	addresses := splitNonEmpty(addrParam, ",")
	if len(addresses) == 0 {
		http.Error(w, "addresses required", http.StatusBadRequest)
		return
	}
	tags, err := tagsFromRequest(r)
	if err != nil {
		writeTaxError(w, err)
		return
	}
	chain := chainParam(q.Get(apiChain))
	start := dateParam(q.Get(apiStart), time.Time{})
	end := dateParam(q.Get(apiEnd), nowUTC().AddDate(0, 0, 1))
	rows, err := s.reviewedRows(chain, addresses, start, end, nil, true)
	if err == nil {
		err = s.loadMessageIndexes(rows)
	}
	if err != nil {
		writeTaxError(w, err)
		return
	}
	migrated, counts, err := MigrateUserTags(rows, tags)
	if err != nil {
		writeTaxError(w, assetReviewInputError{err})
		return
	}
	writeJSON(w, map[string]any{apiTags: migrated, "migration": counts})
}

func handleTagVocabulary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{apiTags: TagVocabulary(), "key_forms": []string{"<tx_hash>", "<tx_hash>:<denom>", "<tx_hash>#<message_index>#<sub_index>"}})
}

// koinlyTagLabel maps a user tag to Koinly's label vocabulary.
func koinlyTagLabel(r Row) (string, bool) {
	switch r.UserTag {
	case TagInternalTransfer:
		return "", true
	case TagIncome, TagAirdrop:
		if r.Direction == "in" {
			return map[string]string{TagIncome: TagIncome, TagAirdrop: TagAirdrop}[r.UserTag], true
		}
	case TagPayment:
		if r.Direction == "in" {
			return TagIncome, true
		}
		return "", true
	case TagGift:
		if r.Direction == apiOut {
			return TagGift, true
		}
		return "", true
	case TagExpense:
		return apiCost, true
	case TagLost:
		return TagLost, true
	}
	return "", false
}

// ctcTagType maps a user tag to Crypto Tax Calculator's type vocabulary.
func ctcTagType(r Row) (string, bool) {
	in := r.Direction == "in"
	switch r.UserTag {
	case TagInternalTransfer:
		if in {
			return ctcTransferIn, true
		}
		return ctcTransferOut, true
	case TagIncome:
		if in {
			return TagIncome, true
		}
	case TagAirdrop:
		if in {
			return TagAirdrop, true
		}
	case TagPayment:
		if in {
			return TagIncome, true
		}
		return apiSend, true
	case TagGift:
		if in {
			return "incoming-gift", true
		}
		return "outgoing-gift", true
	case TagExpense:
		return TagExpense, true
	case TagLost:
		return TagLost, true
	}
	return "", false
}

// coinLedgerType maps a row to CoinLedger's fixed Type vocabulary.
func coinLedgerType(r Row) string {
	switch r.UserTag {
	case TagIncome, TagPayment:
		if r.Direction == "in" {
			return apiIncomeLabel
		}
	case TagAirdrop:
		if r.Direction == "in" {
			return "Airdrop"
		}
	case TagGift:
		return "Gift"
	case TagLost:
		return "Lost"
	}
	switch r.Category {
	case apiReward, apiCommission:
		return "Staking"
	case TagIncome:
		return apiIncomeLabel
	}
	if r.Direction == apiOut {
		return "Withdrawal"
	}
	return "Deposit"
}

// coinTrackerTag maps a row to CoinTracker's Tag column.
func coinTrackerTag(r Row) string {
	switch r.UserTag {
	case TagIncome, TagPayment, TagExpense:
		return TagPayment
	case TagAirdrop:
		return TagAirdrop
	case TagGift:
		return TagGift
	case TagLost:
		return TagLost
	}
	switch r.Category {
	case apiReward, apiCommission:
		return apiStaked
	case TagIncome:
		return TagPayment
	case TagExpense:
		return TagPayment
	}
	return ""
}

// isIncomeRow reports whether a row belongs on the income report: staking
// rewards and commission, protocol income, and receipts the user tagged as
// income, payment or airdrop.
func isIncomeRow(r Row) bool {
	if r.UserTag == TagSpam || r.UserTag == TagInternalTransfer || r.UserTag == TagProtocolDeposit {
		return false
	}
	switch r.Category {
	case apiReward, apiCommission, TagIncome:
		return true
	}
	if r.Direction == "in" {
		switch r.UserTag {
		case TagIncome, TagPayment, TagAirdrop:
			return true
		}
	}
	return false
}

func incomeType(r Row) string {
	if r.Category == TagIncome {
		return "income:" + r.RewardTrigger
	}
	if r.UserTag != "" {
		return "tagged:" + r.UserTag
	}
	return r.Category
}

func incomeSource(r Row) string {
	if r.Category == apiReward || r.Category == apiCommission {
		return apiStaking
	}
	if r.Category == TagIncome {
		return protocolName(r.Detail)
	}
	return r.UserTag
}

// protocolName pulls the protocol display name out of a Detail JSON.
func protocolName(detail string) string {
	if detail == "" {
		return ""
	}
	var m map[string]string
	if json.Unmarshal([]byte(detail), &m) != nil {
		return ""
	}
	if m["protocol"] != "" {
		return m["protocol"]
	}
	return m["contract"]
}

// swapKey groups the legs of one swap. A transaction's legs share its hash.
// Block rows have no hash, so their legs share the swap_group the parser
// put in Detail, and a block row without one stands alone.
func swapKey(r Row) string {
	if r.TxHash != "" || r.Detail == "" {
		return r.TxHash
	}
	var m map[string]string
	if json.Unmarshal([]byte(r.Detail), &m) != nil {
		return ""
	}
	if m["swap_group"] != "" {
		return "block:" + m["swap_group"]
	}
	if m["block_event_id"] != "" {
		return "block_event:" + m["block_event_id"]
	}
	return ""
}

// implySwapPrices values the unpriced leg of a swap from the priced leg in
// the same transaction. Vault shares and freshly listed tokens rarely have an
// oracle price, but the ATOM the user gave up for them does, and that is the
// acquisition cost a controller books.
func implySwapPrices(rows []Row) {
	byTx := map[string][]int{}
	for i, r := range rows {
		if r.Category == apiSwap {
			byTx[swapKey(r)] = append(byTx[swapKey(r)], i)
		}
	}
	for _, idx := range byTx {
		var priced, unpriced []int
		for _, i := range idx {
			if rows[i].PriceMissing {
				unpriced = append(unpriced, i)
			} else {
				priced = append(priced, i)
			}
		}
		if len(priced) == 0 || len(unpriced) == 0 {
			continue
		}
		// Only when the priced side is one direction and the unpriced side
		// the other: value given up equals value received.
		var in, out decimal.Decimal
		dir := ""
		for _, i := range priced {
			if dir == "" {
				dir = rows[i].Direction
			} else if dir != rows[i].Direction {
				dir = "mixed"
			}
			if rows[i].Direction == "in" {
				in = in.Add(rows[i].ValueUSD)
			} else {
				out = out.Add(rows[i].ValueUSD)
			}
		}
		if dir == "mixed" || dir == "" {
			continue
		}
		total := out
		if dir == "in" {
			total = in
		}
		var qty decimal.Decimal
		for _, i := range unpriced {
			if rows[i].Direction == dir {
				qty = decimal.Zero
				break
			}
			qty = qty.Add(rows[i].Amount)
		}
		if !qty.IsPositive() || !total.IsPositive() {
			continue
		}
		unit := total.DivRound(qty, 18)
		for _, i := range unpriced {
			rows[i].PriceUSD = unit
			rows[i].ValueUSD = rows[i].Amount.Mul(unit)
			rows[i].PriceMissing = false
			rows[i].PriceImplied = true
		}
	}
}
