package taxapi

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// These lock in the exact column order/date format/enum strings each vendor's
// importer expects (INF-211: spot-verified against each vendor's current
// published import spec, since a plausible-looking header that's subtly
// wrong silently fails or mis-imports on their end, not ours).

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

func TestWriteCSVCoinTrackerFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, "cointracker", []Row{rewardRow()}); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if lines[0] != "Date,Received Quantity,Received Currency,Sent Quantity,Sent Currency,Fee Amount,Fee Currency,Tag" {
		t.Fatalf("header mismatch: %q", lines[0])
	}
	// CoinTracker requires MM/DD/YYYY and the literal tag "staked" for rewards.
	if !strings.HasPrefix(lines[1], "03/15/2026 10:30:00,1.5,ATOM,,,,,staked") {
		t.Fatalf("row mismatch: %q", lines[1])
	}
}

func TestWriteCSVCoinLedgerFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, "coinledger", []Row{rewardRow()}); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if lines[0] != "Date (UTC),Platform,Asset Sent,Amount Sent,Asset Received,Amount Received,Fee Currency,Fee Amount,Type,Description,TxHash" {
		t.Fatalf("header mismatch: %q", lines[0])
	}
	// CoinLedger's Universal Manual Import requires Month-Day-Year, and "Type"
	// must be one of its fixed vocabulary values ("Staking", not "Staking Reward").
	if !strings.HasPrefix(lines[1], "03/15/2026 10:30:00,Cosmos,,,ATOM,1.5,,,Staking,") {
		t.Fatalf("row mismatch: %q", lines[1])
	}
}

func TestWriteCSVBitwaveFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, "bitwave", []Row{rewardRow()}); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if lines[0] != "id,date,type,amount,amountTicker,txHash,contactAddress,category" {
		t.Fatalf("header mismatch: %q", lines[0])
	}
}
