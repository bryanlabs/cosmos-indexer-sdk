package taxapi

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
)

func protoRow(category, dir, symbol string, amt, price float64, hash string) Row {
	r := Row{
		Time: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), Category: category, Direction: dir,
		Symbol: symbol, Denom: strings.ToLower(symbol), Amount: decimal.NewFromFloat(amt),
		TxHash: hash,
	}
	if price > 0 {
		r.PriceUSD = decimal.NewFromFloat(price)
		r.ValueUSD = r.Amount.Mul(r.PriceUSD)
	} else {
		r.PriceMissing = true
	}
	return r
}

func TestRecordRowsStayOutOfTaxImportsButInGeneric(t *testing.T) {
	rows := []Row{
		protoRow(apiDelegate, apiOut, apiSymbolATOM, 100, 5, "H1"),
		protoRow(TagProtocolDeposit, apiOut, apiSymbolATOM, 10, 5, "H2"),
		protoRow(apiReward, "in", apiSymbolATOM, 1, 5, "H3"),
		protoRow(TagIncome, "in", apiSymbolATOM, 2, 5, "H4"),
		protoRow(TagExpense, apiOut, apiSymbolATOM, 3, 5, "H5"),
	}
	rows[3].RewardTrigger = "tribute"
	for _, format := range []string{apiKoinly, apiCointracker, apiCoinledger, apiCryptotaxcalculator, apiCryptio, apiBitwave} {
		records := readExport(t, format, rows)
		if len(records) != 4 {
			t.Fatalf("%s: got %d records, want header plus three taxable rows", format, len(records))
		}
	}
	generic := readExport(t, apiGeneric, rows)
	if len(generic) != 6 {
		t.Fatalf("generic should keep every row, got %d", len(generic))
	}
	if generic[1][23] != apiFalse || generic[3][23] != apiTrue {
		t.Fatalf("taxable column wrong: %q %q", generic[1][23], generic[3][23])
	}
	koinly := readExport(t, apiKoinly, rows)
	labels := map[string]bool{}
	for _, rec := range koinly[1:] {
		labels[rec[9]] = true
	}
	if !labels[TagIncome] || !labels[apiCost] || !labels[apiStaking] {
		t.Fatalf("koinly labels = %v", labels)
	}
	ctc := readExport(t, apiCryptotaxcalculator, []Row{rows[0]})
	if len(ctc) != 1 {
		t.Fatalf("delegate record must not reach the CTC tax import, got %d rows", len(ctc))
	}
}

func TestPreviewCSVCarriesCategoryTaxableAndDetail(t *testing.T) {
	row := protoRow("hydro_lock_placeholder", apiOut, apiSymbolATOM, 1, 5, "H9")
	row.Category = TagProtocolDeposit
	row.RewardTrigger = "hydro_lock"
	row.Detail = `{"lock_id":"3000","protocol":"Hydro"}`
	var buf bytes.Buffer
	if err := WritePreviewCSV(&buf, apiSumm, []Row{row}); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	header := records[0]
	idx := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		t.Fatalf("missing column %s in %v", name, header)
		return -1
	}
	if records[1][idx("Category")] != TagProtocolDeposit || records[1][idx(apiTaxable)] != apiFalse || records[1][idx(apiPreviewDetail)] != row.Detail || records[1][idx("Source")] != "hydro_lock" {
		t.Fatalf("preview columns wrong: %v", records[1])
	}
}

func TestImplySwapPricesValuesTheUnpricedLeg(t *testing.T) {
	rows := []Row{
		protoRow(apiSwap, apiOut, apiSymbolATOM, 13.5, 4, "S1"),
		protoRow(apiSwap, "in", "INFLOW_SHARE", 11.485382, 0, "S1"),
		protoRow(apiSwap, apiOut, apiSymbolATOM, 1, 0, "S2"), // both legs unpriced: untouched
		protoRow(apiSwap, "in", "X", 1, 0, "S2"),
	}
	implySwapPrices(rows)
	if rows[1].PriceMissing || !rows[1].PriceImplied {
		t.Fatalf("share leg should be implied: %+v", rows[1])
	}
	if !rows[1].ValueUSD.Round(6).Equal(decimal.NewFromFloat(54)) {
		t.Fatalf("share leg value = %s, want 54", rows[1].ValueUSD)
	}
	if !rows[3].PriceMissing {
		t.Fatal("unpriced pair must stay unpriced")
	}
}

func TestUserTagsChangeLabelsAnd8949(t *testing.T) {
	rows := []Row{
		protoRow(apiReward, "in", apiSymbolATOM, 10, 5, "R1"),
		protoRow(apiTransfer, apiOut, apiSymbolATOM, 4, 6, "T1"),
		protoRow(apiTransfer, apiOut, apiSymbolATOM, 4, 6, "T2"),
		protoRow(apiTransfer, "in", "JUNK", 4, 0, "T3"),
	}
	rows[1].Time = rows[0].Time.Add(time.Hour)
	rows[2].Time = rows[0].Time.Add(2 * time.Hour)
	tags, err := ParseUserTags(`{"t1":"internal_transfer","T3":"spam","T2:atom":"gift"}`)
	if err != nil {
		t.Fatal(err)
	}
	rows = ApplyTags(rows, tags)
	if rows[1].UserTag != TagInternalTransfer || rows[2].UserTag != TagGift || rows[3].UserTag != TagSpam {
		t.Fatalf("tags not applied: %+v", rows)
	}
	form := Build8949(rows)
	if len(form) != 1 || !strings.Contains(form[0].Description, "4 ATOM") {
		t.Fatalf("only the gift should reach 8949 as a disposal, got %+v", form)
	}
	koinly := readExport(t, apiKoinly, rows)
	if len(koinly) != 4 { // header + reward + internal + gift; spam excluded
		t.Fatalf("koinly rows = %d", len(koinly))
	}
	if koinly[3][9] != TagGift || koinly[2][9] != "" {
		t.Fatalf("koinly labels: %q %q", koinly[2][9], koinly[3][9])
	}
	if _, err := ParseUserTags(`{"a":"nonsense"}`); err == nil {
		t.Fatal("unknown tag must be rejected")
	}
}

func TestJournalBalancesAndPositionsTrackOpenLots(t *testing.T) {
	rows := []Row{
		protoRow(apiReward, "in", apiSymbolATOM, 10, 5, "R1"),
		protoRow(apiDelegate, apiOut, apiSymbolATOM, 6, 5, "D1"),
		protoRow(apiFee, apiOut, apiSymbolATOM, 0.01, 5, "D1"),
		protoRow(TagIncome, "in", apiSymbolATOM, 2, 5, "I1"),
		protoRow(apiTransfer, apiOut, apiSymbolATOM, 1, 8, "T1"),
	}
	rows[3].RewardTrigger = "tribute"
	for i := range rows {
		rows[i].Time = rows[0].Time.Add(time.Duration(i) * time.Hour)
	}
	lines := BuildJournal(rows, undisclosedAlways)
	var debit, credit decimal.Decimal
	for _, l := range lines {
		if l.Debit != "" {
			d, _ := decimal.NewFromString(l.Debit)
			debit = debit.Add(d)
		}
		if l.Credit != "" {
			c, _ := decimal.NewFromString(l.Credit)
			credit = credit.Add(c)
		}
	}
	if !debit.Equal(credit) || debit.IsZero() {
		t.Fatalf("journal out of balance: %s vs %s", debit, credit)
	}
	accounts := map[string]bool{}
	for _, l := range lines {
		accounts[l.Account] = true
	}
	for _, want := range []string{"Income: Staking Rewards", "Digital Assets Staked: ATOM", "Expense: Transaction Fees", "Income: Protocol: Tribute", "Transfers Out Clearing"} {
		if !accounts[want] {
			t.Fatalf("missing account %q in %v", want, accounts)
		}
	}
	positions := BuildPositions(rows, rows[4].Time.Add(time.Hour), func(string) (decimal.Decimal, bool) { return decimal.NewFromInt(10), true })
	if len(positions) != 1 {
		t.Fatalf("positions = %+v", positions)
	}
	p := positions[0]
	// 10 + 2 acquired, 0.01 fee and 1 transfer disposed; the delegate record is not a disposal.
	if !p.Quantity.Equal(decimal.NewFromFloat(10.99)) {
		t.Fatalf("quantity = %s, want 10.99", p.Quantity)
	}
	if !p.CostBasisUSD.Equal(decimal.NewFromFloat(54.95)) { // 10.99 * 5
		t.Fatalf("basis = %s", p.CostBasisUSD)
	}
	if !p.UnrealizedGainUSD.Equal(decimal.NewFromFloat(54.95)) { // 109.9 - 54.95
		t.Fatalf("unrealized = %s", p.UnrealizedGainUSD)
	}
}

func TestCatalogCoversEveryCategory(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range tax.Catalog() {
		if c.Description == "" {
			t.Fatalf("category %s has no description", c.Category)
		}
		seen[c.Category] = true
	}
	for _, c := range tax.AllCategories() {
		if !seen[string(c)] {
			t.Fatalf("category %s missing from catalog", c)
		}
	}
	if undisclosedAlways("cosmosvaloper1anonymous") != undisclosedJurisdiction {
		t.Fatal("validators without an attestation must read undisclosed")
	}
}
