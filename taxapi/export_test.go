package taxapi

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// These lock in the exact column order/date format/enum strings each vendor's
// importer expects. Every export is parsed with encoding/csv so quoted notes and
// embedded newlines cannot silently shift import columns.
func rewardRow() Row {
	return Row{
		Time:      time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC),
		Category:  "reward",
		Direction: "in",
		Symbol:    "ATOM",
		Denom:     "uatom",
		Amount:    decimal.NewFromFloat(1.5),
		PriceUSD:  decimal.NewFromFloat(8),
		ValueUSD:  decimal.NewFromFloat(12),
		TxHash:    "ABC123",
	}
}

func readExport(t *testing.T, format string, rows []Row) [][]string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteCSV(&buf, format, rows); err != nil {
		t.Fatalf("WriteCSV(%s): %v", format, err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parse %s CSV: %v\n%s", format, err, buf.String())
	}
	return records
}

func TestWriteCSVImportContractsAndEscaping(t *testing.T) {
	row := rewardRow()
	row.ValidatorAddress = "cosmosvaloper1validatorone"
	row.ValidatorMoniker = "Hostile \"validator\"\nname"
	row.RewardTrigger = "claim"
	row.From = "cosmos1from,with\nnewline"
	row.DecimalsAssumed = true
	row.PriceMissing = true

	// The matching tx hash must not cause exporters to collapse rewards from
	// separate validators, such as the two auto-withdrawals in a redelegation.
	other := row
	other.ValidatorAddress = "cosmosvaloper1validatortwo"
	other.ValidatorMoniker = "Second validator"
	other.RewardTrigger = "redelegate"

	formats := map[string]struct {
		header []string
		width  int
		desc   int // -1 means this fixed schema has no description field
	}{
		"koinly": {
			header: []string{"Date", "Sent Amount", "Sent Currency", "Received Amount", "Received Currency", "Fee Amount", "Fee Currency", "Net Worth Amount", "Net Worth Currency", "Label", "Description", "TxHash"},
			width:  12, desc: 10,
		},
		"cointracker": {
			header: []string{"Date", "Received Quantity", "Received Currency", "Sent Quantity", "Sent Currency", "Fee Amount", "Fee Currency", "Tag"},
			width:  8, desc: -1,
		},
		"coinledger": {
			header: []string{"Date (UTC)", "Platform", "Asset Sent", "Amount Sent", "Asset Received", "Amount Received", "Fee Currency", "Fee Amount", "Type", "Description", "TxHash"},
			width:  11, desc: 9,
		},
		"summ": {
			header: []string{"Timestamp (UTC)", "Type", "Base Currency", "Base Amount", "Quote Currency (Optional)", "Quote Amount (Optional)", "Fee Currency (Optional)", "Fee Amount (Optional)", "From (Optional)", "To (Optional)", "Blockchain (Optional)", "ID (Optional)", "Description (Optional)", "Reference Price Per Unit (Optional)", "Reference Price Currency (Optional)"},
			width:  15, desc: 12,
		},
		"cryptotaxcalculator": {
			header: []string{"Timestamp (UTC)", "Type", "Base Currency", "Base Amount", "Quote Currency (Optional)", "Quote Amount (Optional)", "Fee Currency (Optional)", "Fee Amount (Optional)", "From (Optional)", "To (Optional)", "Blockchain (Optional)", "ID (Optional)", "Description (Optional)", "Reference Price Per Unit (Optional)", "Reference Price Currency (Optional)"},
			width:  15, desc: 12,
		},
		"cryptio": {
			header: []string{"transactionDate", "orderType", "txhash", "incomingAsset", "incomingVolume", "incomingUnitRate", "incomingTransactionValue", "outgoingAsset", "outgoingVolume", "outgoingUnitRate", "outgoingTransactionValue", "feeAsset", "feeVolume", "feeUnitRate", "feeTransactionValue", "otherParties", "note"},
			width:  17, desc: 16,
		},
		"bitwave": {
			header: []string{"id", "date", "type", "amount", "amountTicker", "txHash", "contactAddress", "category"},
			width:  8, desc: 7,
		},
		"generic": {
			header: []string{"date_utc", "tx_hash", "category", "direction", "asset", "denom", "amount", "unit_price_usd", "value_usd", "from", "to", "nft_asset", "chain", "decimals_assumed", "price_missing", "validator_address", "validator_moniker", "reward_trigger", "asset_identity"},
			width:  19, desc: -1,
		},
	}

	for format, want := range formats {
		t.Run(format, func(t *testing.T) {
			records := readExport(t, format, []Row{row, other})
			if len(records) != 3 {
				t.Fatalf("record count = %d, want header plus two validator rows", len(records))
			}
			if got := records[0]; !sameFields(got, want.header) {
				t.Fatalf("header = %#v, want %#v", got, want.header)
			}
			for i, record := range records {
				if len(record) != want.width {
					t.Fatalf("record %d width = %d, want %d: %#v", i, len(record), want.width, record)
				}
			}

			if want.desc >= 0 {
				assertStakingDescription(t, records[1][want.desc], row)
				assertStakingDescription(t, records[2][want.desc], other)
			} else if format == "cointracker" {
				// This is intentionally the vendor's fixed eight-column schema.
				// It has no note field in which metadata could be carried.
				if strings.Contains(strings.Join(records[1], "|"), "staking_metadata") {
					t.Fatal("CoinTracker must not gain a metadata field")
				}
			} else { // generic has explicit trailing attribution columns.
				if records[1][15] != row.ValidatorAddress || records[2][15] != other.ValidatorAddress {
					t.Fatalf("generic validator addresses = %q, %q", records[1][15], records[2][15])
				}
				if records[1][16] != row.ValidatorMoniker || records[1][17] != row.RewardTrigger {
					t.Fatalf("generic metadata = %#v", records[1][15:])
				}
			}
		})
	}
}

func TestGenericCSVProtectsValidatorMonikerFormula(t *testing.T) {
	for _, moniker := range []string{"=SUM(A1:A2)", "+cmd", "-1+1", "@cmd", "\tcmd", "\rcmd", "\ncmd"} {
		t.Run(moniker, func(t *testing.T) {
			row := rewardRow()
			row.ValidatorAddress = "cosmosvaloper1validator"
			row.ValidatorMoniker = moniker
			records := readExport(t, "generic", []Row{row})
			if got, want := records[1][16], "'"+moniker; got != want {
				t.Fatalf("validator moniker = %q, want %q", got, want)
			}
		})
	}
}

func TestWriteCSVGenericRetainsSameTransactionValidatorRows(t *testing.T) {
	first := rewardRow()
	first.TxHash = "same-transaction"
	first.ValidatorAddress = "cosmosvaloper1first"
	first.ValidatorMoniker = "First"
	first.RewardTrigger = "redelegate"
	second := first
	second.ValidatorAddress = "cosmosvaloper1second"
	second.ValidatorMoniker = "Second"

	records := readExport(t, "generic", []Row{first, second})
	if len(records) != 3 || records[1][1] != records[2][1] {
		t.Fatalf("same transaction rows were not retained: %#v", records)
	}
	if records[1][15] == records[2][15] || records[1][16] == records[2][16] {
		t.Fatalf("validator attribution collapsed: %#v", records[1:])
	}
}

func assertStakingDescription(t *testing.T, description string, row Row) {
	t.Helper()
	const decimalsWarning = " (decimals unknown, amount may be wrong)"
	const priceWarning = " (price missing)"
	if !strings.HasSuffix(description, decimalsWarning+priceWarning) {
		t.Fatalf("warning suffixes lost or reordered: %q", description)
	}
	metadataStart := strings.Index(description, " | staking_metadata=")
	if metadataStart < 0 {
		t.Fatalf("staking metadata missing: %q", description)
	}
	raw := strings.TrimSuffix(description[metadataStart+len(" | staking_metadata="):], decimalsWarning+priceWarning)
	var metadata struct {
		ValidatorAddress string `json:"validator_address"`
		ValidatorMoniker string `json:"validator_moniker"`
		RewardTrigger    string `json:"reward_trigger"`
	}
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		t.Fatalf("metadata is not JSON: %v (%q)", err, raw)
	}
	if metadata.ValidatorAddress != row.ValidatorAddress || metadata.ValidatorMoniker != row.ValidatorMoniker || metadata.RewardTrigger != row.RewardTrigger {
		t.Fatalf("metadata = %#v, want row %#v", metadata, row)
	}
}

func sameFields(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
