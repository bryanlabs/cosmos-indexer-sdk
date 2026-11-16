package observeapp

// Classification follows the actual native call and its enclosing causal
// keeper scope. Unknown effects are not tax rows, residuals or guessed rewards.
func movementClass(label string) (string, bool) {
	switch label {
	case "delegator_reward/bank.add", "distribution/delegation_reward/bank.add":
		return "delegator_reward_payout", true
	case "validator_commission/bank.add", "distribution/validator_commission/bank.add", "distribution/removed_validator_commission/bank.add":
		return "validator_commission_payout", true
	case "staking/delegate/bank.delegate", "staking/create_validator/bank.delegate":
		return "staking_principal_debit", true
	case "staking/unbonding_complete/bank.undelegate":
		return "unbonding_principal_return", true
	case "staking/undelegate/bank.undelegate":
		return "immediate_unbond_principal_return", true
	case "bank/send/bank.send", "bank/multisend/bank.input_output":
		return "bank_transfer", true
	case "gov/gov/submit_proposal/bank.send", "gov/gov/deposit/bank.send":
		return "governance_deposit", true
	case "gov/deposit_refund/bank.send":
		return "governance_deposit_refund", true
	case "gov/deposit_burn/bank.send":
		return "governance_deposit_burn", true
	}
	return "unsupported_unknown_native_effect", false
}
