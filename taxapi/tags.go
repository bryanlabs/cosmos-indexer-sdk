package taxapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

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
		{"tag": TagInternalTransfer, "label": "Internal transfer", "treatment": "Not a disposal or acquisition. Sent to tax software as a transfer so it matches the other wallet. Left out of Form 8949."},
		{"tag": TagPayment, "label": "Payment", "treatment": "Sent coins are a disposal at fair market value. Received coins are income at receipt and that value is their cost basis."},
		{"tag": TagIncome, "label": "Income", "treatment": "Ordinary income at the USD value on receipt; that value is the cost basis."},
		{"tag": TagGift, "label": "Gift", "treatment": "A gift sent is not a sale. A gift received carries the giver's basis, which we cannot see, so a later disposal shows basis unknown."},
		{"tag": TagAirdrop, "label": "Airdrop", "treatment": "Ordinary income at the USD value on receipt."},
		{"tag": TagProtocolDeposit, "label": "Protocol deposit", "treatment": "Still owned. Kept out of tax-software imports and Form 8949, shown in the Generic CSV and journal."},
		{"tag": TagExpense, "label": "Expense", "treatment": "A disposal at fair market value, labelled as a cost or expense for tax software."},
		{"tag": TagSpam, "label": "Spam", "treatment": "Excluded from tax-software imports and Form 8949. Kept in the Generic CSV for the audit trail."},
		{"tag": TagLost, "label": "Lost or stolen", "treatment": "Labelled lost for tax software. Whether a loss is deductible depends on jurisdiction."},
	}
}

var validTags = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range TagVocabulary() {
		m[t["tag"]] = true
	}
	return m
}()

// UserTags maps a transaction hash, or "<hash>:<denom>" for one asset within
// a transaction, to a tag.
type UserTags map[string]string

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
		out[strings.ToUpper(strings.SplitN(k, ":", 2)[0])+denomSuffix(k)] = v
	}
	return out, nil
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
	if len(r.URL.Query()["tags"]) > 1 {
		return nil, assetReviewInputError{fmt.Errorf("tags must be supplied once")}
	}
	t, err := ParseUserTags(r.URL.Query().Get("tags"))
	if err != nil {
		return nil, assetReviewInputError{err}
	}
	return t, nil
}

// ApplyTags stamps UserTag on the rows a tag addresses. A denom-specific tag
// wins over a transaction-wide one. Fees are never tagged.
func ApplyTags(rows []Row, tags UserTags) []Row {
	if len(tags) == 0 {
		return rows
	}
	for i := range rows {
		if rows[i].Category == "fee" {
			continue
		}
		hash := strings.ToUpper(rows[i].TxHash)
		if t, ok := tags[hash+":"+rows[i].Denom]; ok {
			rows[i].UserTag = t
		} else if t, ok := tags[hash]; ok {
			rows[i].UserTag = t
		}
	}
	return rows
}

func handleTagVocabulary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"tags": TagVocabulary(), "key_forms": []string{"<tx_hash>", "<tx_hash>:<denom>"}})
}

// koinlyTagLabel maps a user tag to Koinly's label vocabulary.
func koinlyTagLabel(r Row) (string, bool) {
	switch r.UserTag {
	case TagInternalTransfer:
		return "", true
	case TagIncome, TagAirdrop:
		if r.Direction == "in" {
			return map[string]string{TagIncome: "income", TagAirdrop: "airdrop"}[r.UserTag], true
		}
	case TagPayment:
		if r.Direction == "in" {
			return "income", true
		}
		return "", true
	case TagGift:
		if r.Direction == "out" {
			return "gift", true
		}
		return "", true
	case TagExpense:
		return "cost", true
	case TagLost:
		return "lost", true
	}
	return "", false
}

// ctcTagType maps a user tag to Crypto Tax Calculator's type vocabulary.
func ctcTagType(r Row) (string, bool) {
	in := r.Direction == "in"
	switch r.UserTag {
	case TagInternalTransfer:
		if in {
			return "transfer-in", true
		}
		return "transfer-out", true
	case TagIncome:
		if in {
			return "income", true
		}
	case TagAirdrop:
		if in {
			return "airdrop", true
		}
	case TagPayment:
		if in {
			return "income", true
		}
		return "send", true
	case TagGift:
		if in {
			return "incoming-gift", true
		}
		return "outgoing-gift", true
	case TagExpense:
		return "expense", true
	case TagLost:
		return "lost", true
	}
	return "", false
}

// coinLedgerType maps a row to CoinLedger's fixed Type vocabulary.
func coinLedgerType(r Row) string {
	switch r.UserTag {
	case TagIncome, TagPayment:
		if r.Direction == "in" {
			return "Income"
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
	case "reward", "commission":
		return "Staking"
	case "income":
		return "Income"
	}
	if r.Direction == "out" {
		return "Withdrawal"
	}
	return "Deposit"
}

// coinTrackerTag maps a row to CoinTracker's Tag column.
func coinTrackerTag(r Row) string {
	switch r.UserTag {
	case TagIncome, TagPayment, TagExpense:
		return "payment"
	case TagAirdrop:
		return "airdrop"
	case TagGift:
		return "gift"
	case TagLost:
		return "lost"
	}
	switch r.Category {
	case "reward", "commission":
		return "staked"
	case "income":
		return "payment"
	case "expense":
		return "payment"
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
	case "reward", "commission", "income":
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
	if r.Category == "income" {
		return "income:" + r.RewardTrigger
	}
	if r.UserTag != "" {
		return "tagged:" + r.UserTag
	}
	return r.Category
}

func incomeSource(r Row) string {
	if r.Category == "reward" || r.Category == "commission" {
		return "staking"
	}
	if r.Category == "income" {
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

// implySwapPrices values the unpriced leg of a swap from the priced leg in
// the same transaction. Vault shares and freshly listed tokens rarely have an
// oracle price, but the ATOM the user gave up for them does, and that is the
// acquisition cost a controller books.
func implySwapPrices(rows []Row) {
	byTx := map[string][]int{}
	for i, r := range rows {
		if r.Category == "swap" {
			byTx[r.TxHash] = append(byTx[r.TxHash], i)
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
