package taxapi

import (
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// GET /validators: every operator the tax index recorded a stake sample for,
// with the current on-chain moniker when the chain still lists it and the
// last indexed one when it does not. The website merges this with the chain's
// own validator list so a validator the chain has since removed can still be
// picked for a statement. The list is read once an hour per chain; the
// distinct scan over tax_validator_rates is cheap but not free.
type indexedValidator struct {
	OperatorAddress string `json:"operator_address"`
	Moniker         string `json:"moniker"`
	FirstHeight     int64  `json:"first_height"`
	LastHeight      int64  `json:"last_height"`
}

var validatorListCache = struct {
	sync.Mutex
	byChain map[string]validatorListEntry
}{byChain: map[string]validatorListEntry{}}

type validatorListEntry struct {
	at   time.Time
	rows []indexedValidator
}

var validatorDescriptionTypes = []string{
	"/cosmos.staking.v1beta1.MsgCreateValidator",
	"/cosmos.staking.v1beta1.MsgEditValidator",
}

type validatorDescriptionMsg struct {
	TypeID int64
	Raw    []byte
}

func (s *Server) indexedValidators(chain string) ([]indexedValidator, error) {
	validatorListCache.Lock()
	defer validatorListCache.Unlock()
	if e, ok := validatorListCache.byChain[chain]; ok && time.Since(e.at) < time.Hour {
		return e.rows, nil
	}
	var rows []indexedValidator
	if err := s.db.Raw(`SELECT operator AS operator_address, min(height) AS first_height, max(height) AS last_height
FROM tax_validator_rates WHERE chain = ? GROUP BY operator`, chain).Scan(&rows).Error; err != nil {
		return nil, err
	}
	// A missing indexed name only leaves a removed validator unnamed, so a
	// failed lookup is logged and the list is still served.
	names, err := s.indexedMonikers(chain)
	if err != nil {
		log.Printf("validators %s: indexed monikers: %v", chain, err) //nolint:gosec // chain is normalized by chainParam
	}
	for i := range rows {
		rows[i].Moniker = names[rows[i].OperatorAddress]
	}
	validatorListCache.byChain[chain] = validatorListEntry{at: time.Now(), rows: rows}
	return rows, nil
}

// indexedMonikers reads every successful MsgCreateValidator and
// MsgEditValidator the index stored for the chain, in block order, and keeps
// each operator's latest moniker. The type ids are resolved first so the
// planner sees literal ids and reads the few thousand rows off the
// message_type_id index (about 250 ms); joined to message_types by name it
// estimates a million rows and scans every message.
func (s *Server) indexedMonikers(chain string) (map[string]string, error) {
	var types []struct {
		ID          int64
		MessageType string
	}
	if err := s.db.Table("message_types").Select("id, message_type").Where("message_type IN ?", validatorDescriptionTypes).Scan(&types).Error; err != nil {
		return nil, err
	}
	if len(types) == 0 {
		return nil, nil
	}
	typeIDs := make([]int64, len(types))
	typeURLs := make(map[int64]string, len(types))
	for i, t := range types {
		typeIDs[i], typeURLs[t.ID] = t.ID, t.MessageType
	}
	var msgs []validatorDescriptionMsg
	if err := s.db.Raw(`SELECT m.message_type_id AS type_id, m.message_bytes AS raw
FROM messages m
JOIN txes t ON t.id = m.tx_id
JOIN blocks b ON b.id = t.block_id
JOIN chains c ON c.id = b.chain_id
WHERE m.message_type_id IN ? AND c.chain_id IN ? AND t.code = 0 AND m.message_bytes IS NOT NULL
ORDER BY b.height, m.tx_id, m.message_index`, typeIDs, chainIDs(chain)).Scan(&msgs).Error; err != nil {
		return nil, err
	}
	return latestMonikers(msgs, typeURLs), nil
}

// latestMonikers replays validator descriptions in block order. An edit that
// sends the "[do-not-modify]" placeholder keeps the previous name, as the
// chain does; a message that does not decode is skipped.
func latestMonikers(msgs []validatorDescriptionMsg, typeURLs map[int64]string) map[string]string {
	names := map[string]string{}
	for _, m := range msgs {
		var operator, moniker string
		switch typeURLs[m.TypeID] {
		case validatorDescriptionTypes[0]:
			var msg stakingtypes.MsgCreateValidator
			if msg.Unmarshal(m.Raw) != nil {
				continue
			}
			operator, moniker = msg.ValidatorAddress, msg.Description.Moniker
		case validatorDescriptionTypes[1]:
			var msg stakingtypes.MsgEditValidator
			if msg.Unmarshal(m.Raw) != nil {
				continue
			}
			operator, moniker = msg.ValidatorAddress, msg.Description.Moniker
		default:
			continue
		}
		if operator == "" || moniker == "" || moniker == stakingtypes.DoNotModifyDesc {
			continue
		}
		names[operator] = moniker
	}
	return names
}

func (s *Server) handleValidatorList(w http.ResponseWriter, r *http.Request) {
	chain := chainParam(r.URL.Query().Get(apiChain))
	rows, err := s.indexedValidators(chain)
	if err != nil {
		http.Error(w, "validator list unavailable", http.StatusServiceUnavailable)
		return
	}
	monikers := s.oracle.ValidatorMonikers()
	out := make([]indexedValidator, len(rows))
	for i, v := range rows {
		if m := monikers[v.OperatorAddress]; m != "" {
			v.Moniker = m
		}
		out[i] = v
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperatorAddress < out[j].OperatorAddress })
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, map[string]any{apiChain: chain, "count": len(out), "validators": out})
}
