package taxapi

import (
	"encoding/json"
	"sort"
	"time"

	"gorm.io/gorm"

	txlog "github.com/DefiantLabs/cosmos-indexer/cosmos/modules/tx"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/tax"
)

// Attribute values can be long (wasm payloads), so the lookup index is on a
// prefix and every query repeats the full-value comparison.
const blockEventValueIndex = `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_block_event_attr_value ON block_event_attributes (left(value, 100))`

// BlockEventsMigrate creates the index that finds a wallet's block events.
func BlockEventsMigrate(db *gorm.DB) error {
	return db.Exec(blockEventValueIndex).Error
}

type blockLifecycle struct {
	BlockID           uint
	LifecyclePosition models.BlockLifecyclePosition
}

type blockEventAttrRow struct {
	EventID           uint
	BlockID           uint
	LifecyclePosition models.BlockLifecyclePosition
	Height            int64
	TimeStamp         time.Time
	Type              string
	Key               *string
	Value             *string
}

// blockEventRows classifies every block lifecycle in [start,end) that names
// addr, plus the lifecycles that ended a proposal addr deposited on.
func (s *Server) blockEventRows(chain, addr string, start, end time.Time) ([]tax.TaxableEvent, error) {
	scope := func(q *gorm.DB) *gorm.DB {
		return q.Joins("JOIN blocks b ON b.id = be.block_id").
			Joins("JOIN chains c ON c.id = b.chain_id").
			Where("c.chain_id IN ? AND b.time_stamp >= ? AND b.time_stamp < ?", chainIDs(chain), start, end)
	}
	var lifecycles []blockLifecycle
	if err := scope(s.db.Table("block_event_attributes a").
		Select("DISTINCT be.block_id, be.lifecycle_position").
		Joins("JOIN block_events be ON be.id = a.block_event_id")).
		Where("left(a.value, 100) = left(?, 100) AND a.value = ?", addr, addr).
		Scan(&lifecycles).Error; err != nil {
		return nil, err
	}

	deposits, err := s.walletGovDeposits(chain, addr, end)
	if err != nil {
		return nil, err
	}
	if len(deposits) > 0 {
		ids := map[string]bool{}
		for _, d := range deposits {
			ids[d.ProposalID] = true
		}
		proposals := make([]string, 0, len(ids))
		for id := range ids {
			proposals = append(proposals, id)
		}
		var ended []blockLifecycle
		if err := scope(s.db.Table("block_event_attributes a").
			Select("DISTINCT be.block_id, be.lifecycle_position").
			Joins("JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id").
			Joins("JOIN block_events be ON be.id = a.block_event_id").
			Joins("JOIN block_event_types t ON t.id = be.block_event_type_id")).
			Where("k.key = 'proposal_id' AND t.type IN ('active_proposal', 'inactive_proposal')").
			Where("left(a.value, 100) IN ? AND a.value IN ?", proposals, proposals).
			Scan(&ended).Error; err != nil {
			return nil, err
		}
		lifecycles = append(lifecycles, ended...)
	}
	if len(lifecycles) == 0 {
		return nil, nil
	}

	seen := map[blockLifecycle]bool{}
	blockIDs := []uint{}
	for _, l := range lifecycles {
		if !seen[l] {
			seen[l] = true
			blockIDs = append(blockIDs, l.BlockID)
		}
	}
	var attrs []blockEventAttrRow
	if err := s.db.Table("block_events be").
		Select("be.id AS event_id, be.block_id, be.lifecycle_position, b.height, b.time_stamp, t.type, k.key, a.value").
		Joins("JOIN blocks b ON b.id = be.block_id").
		Joins("JOIN block_event_types t ON t.id = be.block_event_type_id").
		Joins("LEFT JOIN block_event_attributes a ON a.block_event_id = be.id").
		Joins("LEFT JOIN block_event_attribute_keys k ON k.id = a.block_event_attribute_key_id").
		Where("be.block_id IN ?", blockIDs).
		Order("be.block_id, be.lifecycle_position, be.index, a.index").
		Scan(&attrs).Error; err != nil {
		return nil, err
	}
	return classifyBlockAttrs(attrs, seen, addr, deposits), nil
}

// classifyBlockAttrs groups ordered attribute rows into lifecycles and
// classifies the ones in want.
func classifyBlockAttrs(attrs []blockEventAttrRow, want map[blockLifecycle]bool, addr string, deposits []tax.GovDeposit) []tax.TaxableEvent {
	var out []tax.TaxableEvent
	var current blockLifecycle
	var events []tax.BlockEvent
	flush := func() {
		if want[current] {
			out = append(out, tax.ClassifyBlockLifecycle(events, addr, deposits)...)
		}
		events = nil
	}
	for i, a := range attrs {
		l := blockLifecycle{BlockID: a.BlockID, LifecyclePosition: a.LifecyclePosition}
		if i == 0 || l != current {
			if i > 0 {
				flush()
			}
			current = l
		}
		if len(events) == 0 || events[len(events)-1].ID != a.EventID {
			phase := tax.BlockPhaseBegin
			if a.LifecyclePosition == models.EndBlockEvent {
				phase = tax.BlockPhaseEnd
			}
			events = append(events, tax.BlockEvent{ID: a.EventID, Height: a.Height, Timestamp: a.TimeStamp, Phase: phase, Type: a.Type})
		}
		if a.Key != nil && a.Value != nil {
			e := &events[len(events)-1]
			e.Attributes = append(e.Attributes, txlog.Attribute{Key: *a.Key, Value: *a.Value})
		}
	}
	if len(attrs) > 0 {
		flush()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out
}

// walletGovDeposits lists addr's governance deposits made before end.
func (s *Server) walletGovDeposits(chain, addr string, end time.Time) ([]tax.GovDeposit, error) {
	var rows []tax.TaxableEvent
	if err := s.scopedTaxEvents(chain).Select("taxable_events.*").
		Where("taxable_events.category = ? AND taxable_events.from_addr = ? AND taxable_events.timestamp < ?", string(tax.CategoryGovDeposit), addr, end).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]tax.GovDeposit, 0, len(rows))
	for _, r := range rows {
		var detail struct {
			ProposalID string `json:"proposal_id"`
		}
		if json.Unmarshal([]byte(r.Detail), &detail) != nil || detail.ProposalID == "" {
			continue
		}
		out = append(out, tax.GovDeposit{ProposalID: detail.ProposalID, Amount: r.Amount, Denom: r.Denom})
	}
	return out, nil
}
