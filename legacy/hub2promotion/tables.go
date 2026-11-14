// Package hub2promotion is a separate, fail-closed atomic promotion candidate.
// It does not change the existing empty-destination importer or ledger writer.
package hub2promotion

import "strings"

const Chain = "cosmoshub-2"
const CanonicalHeight int64 = 2902000
const FinalHeight int64 = 2902002

// Table order is the insertion dependency order, never caller-controlled SQL.
type table struct{ name, columns, key string }

var inserts = []table{
	{"genesis_files", "chain_id,sha256,genesis_time,accounts,delegations,unbonding_entries,validators,withdraw_addresses,genesis_txs,loaded_at", "chain_id"},
	{"genesis_accounts", "chain_id,address,file_position,account_number,sequence,kind,module_name,file_coins,liquid,delegated,unbonding,original_vesting,delegated_free,delegated_vesting,vesting_start,vesting_end", "chain_id,address"},
	{"genesis_delegations", "chain_id,delegator,validator,shares,tokens,from_genesis_tx", "chain_id,delegator,validator"},
	{"genesis_unbondings", "chain_id,delegator,validator,entry,creation_height,completion_time,initial_balance,balance", "chain_id,delegator,validator,entry"},
	{"genesis_validators", "chain_id,operator,tokens,delegator_shares,status,jailed,outstanding_rewards,accumulated_commission", "chain_id,operator"},
	{"genesis_withdraw_addresses", "chain_id,delegator,withdraw_address", "chain_id,delegator"},
	{"blocks", "chain_id,height,time,block_hash,proposer,tx_count", "chain_id,height"},
	{"tx_inclusions", "chain_id,height,tx_index,hash,code,codespace,code_evidence,gas_wanted,gas_used,fee,fee_payer,fee_charged,fee_evidence,memo,signers,raw", "chain_id,height,tx_index"},
	{"block_events", "chain_id,height,phase,event_index,type,attributes", "chain_id,height,phase,event_index"},
	{"taxable_events", "chain_id,height,phase,tx_index,msg_index,sub_index,category,amount,denom,asset,from_addr,to_addr,validator_address,reward_trigger,detail,timestamp,tx_hash,amount_source", "chain_id,height,phase,tx_index,msg_index,sub_index"},
}
var allTables = []table{
	{"balance_days", "", "chain_id,address,day"}, {"block_events", "", "chain_id,height,phase,event_index"},
	{"blocks", "", "chain_id,height"}, {"eras", "", "chain_id"}, {"genesis_accounts", "", "chain_id,address"},
	{"genesis_delegations", "", "chain_id,delegator,validator"}, {"genesis_files", "", "chain_id"},
	{"genesis_unbondings", "", "chain_id,delegator,validator,entry"}, {"genesis_validators", "", "chain_id,operator"},
	{"genesis_withdraw_addresses", "", "chain_id,delegator"}, {"import_ranges", "", "chain_id,from_height"},
	{"reconciliation", "", "chain_id,address,component"}, {"taxable_events", "", "chain_id,height,phase,tx_index,msg_index,sub_index"},
	{"tx_inclusions", "", "chain_id,height,tx_index"},
}

func quotedColumns(s string) string { return `"` + strings.ReplaceAll(s, ",", `","`) + `"` }
func inserted(name string) bool {
	for _, t := range inserts {
		if t.name == name {
			return true
		}
	}
	return false
}
