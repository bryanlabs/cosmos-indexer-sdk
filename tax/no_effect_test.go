package tax

import "testing"

func TestNoTaxEffectTypesAreNotAlsoClassified(t *testing.T) {
	classified := map[string]bool{}
	for _, url := range MessageTypeURLs {
		classified[url] = true
	}
	seen := map[string]bool{}
	for _, url := range NoTaxEffectTypeURLs {
		if classified[url] || seen[url] {
			t.Fatalf("%s is listed twice", url)
		}
		seen[url] = true
	}
}

// Types from the cosmoshub-4 census that move coins are never no-effect; until
// they are classified, a report that touches one is held back.
func TestCoinMovingCensusTypesAreNotNoEffect(t *testing.T) {
	noEffect := map[string]bool{}
	for _, url := range NoTaxEffectTypeURLs {
		noEffect[url] = true
	}
	for _, url := range []string{
		"/ibc.core.channel.v2.MsgSendPacket",
		"/interchain_security.ccv.provider.v1.MsgRegisterConsumerRewardDenom",
		"/tendermint.liquidity.v1beta1.MsgCreatePool",
		"/tendermint.liquidity.v1beta1.MsgDepositWithinBatch",
		"/tendermint.liquidity.v1beta1.MsgWithdrawWithinBatch",
		"/tendermint.liquidity.v1beta1.MsgSwapWithinBatch",
	} {
		if noEffect[url] {
			t.Fatalf("%s moves coins and cannot be no-effect", url)
		}
	}
}
