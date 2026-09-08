package taxapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
)

const airdrop67Voucher = "ibc/F9E07DBD5DC03D88E18DE89CB127E0B7CAF5B9A9893A7FD6148E4CC951D1536C"

func (s *Server) eventMemos(events []tax.TaxableEvent) (map[string]string, error) {
	hashes := []string{}
	seen := map[string]bool{}
	for _, e := range events {
		if e.Denom != "uatom" && !seen[e.TxHash] {
			seen[e.TxHash] = true
			hashes = append(hashes, e.TxHash)
		}
	}
	out := map[string]string{}
	if len(hashes) == 0 {
		return out, nil
	}
	var rows []struct {
		Hash string
		Memo string
	}
	if err := s.db.Table("txes").Select("hash,memo").Where("hash IN ?", hashes).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.Hash] = r.Memo
	}
	return out, nil
}

func assetRecordID(chain, wallet string, event tax.TaxableEvent, identity *AssetIdentity) string {
	facts := []any{event.TxHash, event.Denom, event.Amount, event.Timestamp.UTC().Format(time.RFC3339Nano), event.Category, event.FromAddr, event.ToAddr, identity.ReportedLabel, identity.TokenName, identity.RawDenom, identity.RawAmount, identity.SourceChain, identity.ChannelPath, identity.BaseToken, identity.Memo, identity.OfficialAtomMatch, identity.SuspectedSpam, identity.Reasons}
	data, _ := json.Marshal(facts)
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%s|%s|%d|%d:%x", chain, wallet, event.MessageID, event.SubIndex, digest[:])
}

func attachSpamEvidence(row *Row, event tax.TaxableEvent, chain, wallet, memo string) {
	lower := strings.ToLower(memo)
	promotional := (strings.Contains(lower, "https://") || strings.Contains(lower, "http://")) && (strings.Contains(lower, "claim") || strings.Contains(lower, "airdrop"))
	knownSuspect := chain == "mainnet" && (event.Denom == junoFactoryUatomVoucher || event.Denom == airdrop67Voucher)
	// Missing price alone is not a spam signal, nor is a normal native-token receipt.
	suspicious := knownSuspect || (event.Denom != "uatom" && event.Denom != cosmosHubNobleUSDCDenom && row.PriceMissing && promotional)
	if row.AssetIdentity == nil && !suspicious {
		return
	}
	if row.AssetIdentity == nil {
		row.AssetIdentity = &AssetIdentity{ReportedLabel: row.Symbol, TokenName: row.Symbol, RawDenom: event.Denom, RawAmount: event.Amount, BaseToken: event.Denom, Treatment: "unreviewed"}
	}
	a := row.AssetIdentity
	a.Wallet = wallet
	a.Memo = memo
	a.SuspectedSpam = suspicious
	a.Note = "Review required. The reported label and original transaction remain visible. No valuation or exclusion is applied without your explicit choice."
	if !a.OfficialAtomMatch && strings.EqualFold(a.ReportedLabel, "ATOM") {
		a.Reasons = append(a.Reasons, "The raw denomination did not match official Cosmos Hub uatom; an ATOM label does not prove ATOM backing.")
	}
	if knownSuspect {
		a.Reasons = append(a.Reasons, "This is an issuer-created Juno tokenfactory asset, not native ATOM. No native ATOM backing or reliable market price has been verified.")
	}
	if promotional {
		a.Reasons = append(a.Reasons, "The transaction memo advertises a claim or airdrop at an external website. This is a suspected-spam signal, not proof of value.")
	}
	if event.Denom == airdrop67Voucher {
		a.SourceChain = "juno-1"
		a.ChannelPath = "transfer/channel-207"
		a.BaseToken = "factory/juno1wypsnn7n5hsd2kvk424qv9yuretz9m6k6wcztk/uairdrop67"
	}
	a.RecordID = assetRecordID(chain, wallet, event, a)
	row.PriceMissing = true
	row.PriceUSD = decimal.Zero
	row.ValueUSD = decimal.Zero
}
