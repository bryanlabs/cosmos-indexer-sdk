package ledger

import (
	"context"
	"fmt"
)

// LastLeaves lists the stored messages of a range that could have removed a
// validator, as chain/height/transaction/message. Cosmos Hub 3 removed an
// unbonded validator inside the message that took its last stake, with no
// event. After that nothing can leave the validator until a message creates
// it again, so the message that removed it is always the last one to leave
// it before its next creation, or before the end of the chain.
//
// The list is every such last leave inside the range: an undelegation or a
// redelegation away from a validator that no later message of the range
// leaves before a message of the range creates the validator again or the
// range ends. A range that is not the whole chain lists more messages than
// the chain's own last leaves and never fewer.
//
// It reads the tax rows the importer wrote: one undelegate row for an
// undelegation, one redelegate row naming its source validator for a
// redelegation, and for a created validator the delegate row whose amount
// came from the message.
func LastLeaves(ctx context.Context, db querier, r Range) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
WITH moves AS (
  SELECT height, tx_index, msg_index, 0 AS made, validator_address AS validator
    FROM legacy.taxable_events
   WHERE chain_id = $1 AND height BETWEEN $2 AND $3 AND phase = $4 AND category = 'undelegate'
  UNION
  SELECT height, tx_index, msg_index, 0, (detail::json) ->> 'source_validator'
    FROM legacy.taxable_events
   WHERE chain_id = $1 AND height BETWEEN $2 AND $3 AND phase = $4 AND category = 'redelegate'
  UNION
  SELECT height, tx_index, msg_index, 1, validator_address
    FROM legacy.taxable_events
   WHERE chain_id = $1 AND height BETWEEN $2 AND $3 AND phase = $4 AND category = 'delegate' AND amount_source = 'message'
), ordered AS (
  SELECT moves.*, lead(made) OVER (PARTITION BY validator ORDER BY height, tx_index, msg_index, made) AS next
    FROM moves
)
SELECT height, tx_index, msg_index FROM ordered
 WHERE made = 0 AND (next IS NULL OR next = 1)
 ORDER BY height, tx_index, msg_index`, r.Chain, r.From, r.To, PhaseTx)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var height, tx, msg int64
		if err := rows.Scan(&height, &tx, &msg); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s/%d/%d/%d", r.Chain, height, tx, msg))
	}
	return out, closeRows(rows)
}
