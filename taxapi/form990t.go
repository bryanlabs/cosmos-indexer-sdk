package taxapi

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Form 990-T / UBIT computation for staking income earned inside a tax-advantaged
// account (IRA/HSA/Solo-401k). Staking rewards are treated here as unrelated
// business taxable income (UBTI); whether staking is UBTI is a contested area, so
// this is a conservative estimate, not advice. An IRA is a trust, so 990-T tax is
// computed at trust rates after the $1,000 specific deduction. No 990-T filing is
// required when gross UBTI is under $1,000.

// SpecificDeduction is the §512(b)(12) specific deduction.
var specificDeduction = decimal.NewFromInt(1000)

// trustBracket is a marginal tax bracket.
type trustBracket struct {
	upTo decimal.Decimal // upper bound of the bracket (zero value = no cap)
	rate decimal.Decimal
}

// 2024 trust/estate brackets (inflation-adjusted yearly; used as the estimate).
func trustBrackets() []trustBracket {
	d := decimal.NewFromInt
	pct := func(p int64) decimal.Decimal { return decimal.NewFromInt(p).Div(decimal.NewFromInt(100)) }
	return []trustBracket{
		{upTo: d(3100), rate: pct(10)},
		{upTo: d(11150), rate: pct(24)},
		{upTo: d(15200), rate: pct(35)},
		{upTo: decimal.Zero, rate: pct(37)}, // top, no cap
	}
}

// Form990T is the UBIT summary for an entity's staking income.
type Form990T struct {
	StakingIncomeUSD  string `json:"staking_income_usd"` // gross UBTI
	SpecificDeduction string `json:"specific_deduction"`
	TaxableUBTI       string `json:"taxable_ubti_usd"`
	EstimatedTaxUSD   string `json:"estimated_tax_usd"`
	FilingRequired    bool   `json:"filing_required"` // gross UBTI >= $1,000
	// PriceMissingRows counts staking-income rows with no known price; their
	// value is excluded from StakingIncomeUSD above (0, not a real zero), so the
	// true UBTI is at least this much higher (INF-201).
	PriceMissingRows int `json:"price_missing_rows"`
	// RecognitionPolicy is the policy that timed this income (INF-206); printed
	// on the report so it's never an implicit assumption.
	RecognitionPolicy string `json:"recognition_policy"`
	Note              string `json:"note"`
}

// compute990T runs the UBIT calc over a gross UBTI (USD) already summed by the
// caller; priceMissingRows is the count of income rows that had no known price
// and so contributed 0 to that sum, so the note can say the total is a floor.
func compute990T(ubti decimal.Decimal, priceMissingRows int, recognitionPolicy string) Form990T {
	taxable := ubti.Sub(specificDeduction)
	if taxable.IsNegative() {
		taxable = decimal.Zero
	}
	tax := trustTax(taxable)
	note := "Estimate only. Staking as UBTI is unsettled; tax computed at 2024 trust rates after the $1,000 deduction."
	if priceMissingRows > 0 {
		note += fmt.Sprintf(" %d income row(s) had no known price and are excluded from the total above, actual UBTI is higher.", priceMissingRows)
	}
	return Form990T{
		StakingIncomeUSD:  ubti.StringFixed(2),
		SpecificDeduction: specificDeduction.StringFixed(2),
		TaxableUBTI:       taxable.StringFixed(2),
		EstimatedTaxUSD:   tax.StringFixed(2),
		FilingRequired:    ubti.GreaterThanOrEqual(specificDeduction),
		PriceMissingRows:  priceMissingRows,
		RecognitionPolicy: recognitionPolicy,
		Note:              note,
	}
}

// trustTax applies the marginal trust brackets to a taxable amount.
func trustTax(taxable decimal.Decimal) decimal.Decimal {
	if !taxable.IsPositive() {
		return decimal.Zero
	}
	tax := decimal.Zero
	lower := decimal.Zero
	for _, b := range trustBrackets() {
		var top decimal.Decimal
		if b.upTo.IsZero() {
			top = taxable // top bracket, no cap
		} else {
			top = decimal.Min(taxable, b.upTo)
		}
		if top.GreaterThan(lower) {
			tax = tax.Add(top.Sub(lower).Mul(b.rate))
		}
		lower = b.upTo
		if !b.upTo.IsZero() && taxable.LessThanOrEqual(b.upTo) {
			break
		}
	}
	return tax
}
