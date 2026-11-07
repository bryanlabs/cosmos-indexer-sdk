package db

import "github.com/DefiantLabs/cosmos-indexer/db/models"

// pendingIDCache never exposes uncommitted lookup IDs to another block. Only
// the owner of a successfully committed top-level transaction may publish it.
type pendingIDCache struct {
	committed *idCache
	ids       map[string]uint
}

func newPendingIDCache(committed *idCache) *pendingIDCache {
	return &pendingIDCache{committed: committed, ids: make(map[string]uint)}
}

func (c *pendingIDCache) get(key string) (uint, bool) {
	if id, ok := c.ids[key]; ok {
		return id, true
	}
	return c.committed.get(key)
}

func (c *pendingIDCache) put(key string, id uint) {
	if id != 0 {
		c.ids[key] = id
	}
}

func (c *pendingIDCache) publish() {
	for key, id := range c.ids {
		c.committed.put(key, id)
	}
}

// Raw persistence attaches database IDs throughout these slices. Keep the
// caller's retry input independent of those mutations until commit succeeds.
func cloneBlockTransactions(txs []TxDBWrapper) []TxDBWrapper {
	cloned := append([]TxDBWrapper(nil), txs...)
	for i, tx := range txs {
		cloned[i].Tx.SignerAddresses = append([]models.Address(nil), tx.Tx.SignerAddresses...)
		cloned[i].Tx.Fees = append([]models.Fee(nil), tx.Tx.Fees...)
		cloned[i].Messages = append([]MessageDBWrapper(nil), tx.Messages...)
		for j, message := range tx.Messages {
			cloned[i].Messages[j].MessageEvents = append([]MessageEventDBWrapper(nil), message.MessageEvents...)
			for k, event := range message.MessageEvents {
				cloned[i].Messages[j].MessageEvents[k].Attributes = append([]models.MessageEventAttribute(nil), event.Attributes...)
			}
		}
	}
	return cloned
}
