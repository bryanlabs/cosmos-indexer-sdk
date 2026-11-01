package tax

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/DefiantLabs/cosmos-indexer/config"
	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
)

// protocolFixture loads a real cosmoshub-4 message exported from the
// production index (tax/testdata/<name>.json): its type URL and the full
// event log of that one message.
func protocolFixture(t *testing.T, name string) (string, *txlog.LogMessage) {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		MessageType string                  `json:"message_type"`
		Code        int                     `json:"code"`
		Events      []txlog.LogMessageEvent `json:"events"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Code != 0 {
		t.Fatal("fixture transaction failed")
	}
	return fixture.MessageType, &txlog.LogMessage{Events: fixture.Events}
}

// replayFixture classifies a fixture exactly the way taxrepair replays a
// stored message: rebuild the message from events, then run the parser.
func replayFixture(t *testing.T, name string) []TaxableEvent {
	t.Helper()
	typ, log := protocolFixture(t, name)
	msg, err := ReconstructMsg(typ, log)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out, err := (&Parser{ID: "test"}).ParseMessage(msg, log, cfgForTests())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return (*out).([]TaxableEvent)
}

func summary(events []TaxableEvent) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s|%s|%s|%s\n", e.Category, e.Amount, e.Denom, e.Asset, e.FromAddr, e.ToAddr, e.ValidatorAddress, e.RewardTrigger, e.Detail)
	}
	return b.String()
}

// TestFixtureClassification replays every mainnet fixture and compares the
// resulting rows with the reviewed golden output in testdata/<name>.golden.
// Set UPDATE_GOLDEN=1 to rewrite the golden files after a deliberate change,
// then review the diff: each line is
// category|amount|denom|asset|from|to|validator|trigger|detail.
func TestFixtureClassification(t *testing.T) {
	names, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range names {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		n := strings.TrimSuffix(f.Name(), ".json")
		var raw map[string]any
		data, _ := os.ReadFile("testdata/" + f.Name())
		_ = json.Unmarshal(data, &raw)
		if _, ok := raw["message_type"]; !ok {
			continue
		}
		seen++
		t.Run(n, func(t *testing.T) {
			got := summary(replayFixture(t, n))
			if got == "" {
				t.Fatalf("%s produced no rows", n)
			}
			golden := "testdata/" + n + ".golden"
			if os.Getenv("UPDATE_GOLDEN") != "" {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden file %s (run with UPDATE_GOLDEN=1): %v", golden, err)
			}
			if string(want) != got {
				t.Errorf("%s classification changed\nwant:\n%s\ngot:\n%s", n, want, got)
			}
		})
	}
	if seen < 30 {
		t.Fatalf("only %d fixtures found", seen)
	}
}

func cfgForTests() config.IndexConfig { return config.IndexConfig{} }
