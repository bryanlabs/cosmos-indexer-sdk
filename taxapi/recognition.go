package taxapi

import (
	"encoding/json"
	"net/http"
)

// RecognitionPolicyDefault is the only implemented income-recognition policy
// today. "at-sale" (deferring recognition to disposal) is a named, documented
// extension point (INF-206) for if pending legislation changes when staking
// income is realized — not implemented yet, requests for it are rejected
// rather than silently falling back to at-claim.
const RecognitionPolicyDefault = "at-claim"

// RecognitionPolicy documents when staking/commission income is recognized,
// for the /tax/methodology page and as a citation on income reports.
type RecognitionPolicy struct {
	Policy        string `json:"policy"`
	EffectiveDate string `json:"effective_date"`
	Explanation   string `json:"explanation"`
}

func currentRecognitionPolicy() RecognitionPolicy {
	return RecognitionPolicy{
		Policy:        RecognitionPolicyDefault,
		EffectiveDate: "2026-07-04",
		Explanation: "Staking rewards and validator commission are income when the recipient has " +
			"dominion and control over them (Rev. Rul. 2023-14). On Cosmos, earned rewards accrue " +
			"in the distribution module and are not constructively received until withdrawn: a " +
			"manual claim, an auto-withdraw triggered by delegate/undelegate/redelegate, or a " +
			"REStake auto-compound claim. Before that withdrawal the delegator cannot transfer or " +
			"otherwise exercise control over the reward. We recognize income at that withdrawal " +
			"event (\"at-claim\"), valued at its USD price that day. This is the defensible reading " +
			"of current law for Cosmos, and the architecture is ready for an \"at-sale\" policy " +
			"(deferring recognition to disposal) without reindexing, since every reward's own claim " +
			"date and amount is already stored; only the query-time recognition rule would change. " +
			"Pending legislation (e.g. the Lummis bill, House Ways & Means drafts) could move " +
			"staking rewards to taxed-at-sale; if enacted, that will be added as a selectable policy.",
	}
}

func (s *Server) handleRecognitionPolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(currentRecognitionPolicy())
}
