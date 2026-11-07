package taxapi

// Why a receipt lands in the asset review. The code is stable and machine
// readable; the text is what the website and the generic CSV show. The texts
// of the three original reasons are part of every record fingerprint (see
// assetRecordID), so they must never change; a new reason gets a new code.
const (
	ReasonSpamMemo          = "spam_memo"
	ReasonLabelMismatch     = "label_mismatch"
	ReasonUnverifiedFactory = "unverified_factory"
	ReasonIdentityMismatch  = "identity_mismatch"
)

// ReviewReason is one entry of the glossary served with /asset-review so a
// reader (or an agent) can explain a flag without guessing.
type ReviewReason struct {
	Code      string `json:"code"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	Explainer string `json:"explainer"`
	Suggested string `json:"suggested"`
}

var reviewReasons = []ReviewReason{
	{
		Code:      ReasonSpamMemo,
		Title:     "Promotional memo",
		Text:      "The transaction memo advertises a claim or airdrop at an external website. This is a suspected-spam signal, not proof of value.",
		Explainer: "Unsolicited tokens sent with a memo that points at a claim site are the usual pattern for phishing airdrops. They arrive without you doing anything, usually have no market, and the site is the actual attack. Receiving one is not income if it has no value, but only you can confirm you never claimed or sold anything through it.",
		Suggested: "Valueless, unless you know the token and it traded at a price when it arrived.",
	},
	{
		Code:      ReasonLabelMismatch,
		Title:     "Label says ATOM, token is not ATOM",
		Text:      "The raw denomination did not match official Cosmos Hub uatom; an ATOM label does not prove ATOM backing.",
		Explainer: "The sender labelled the token ATOM, but the on-chain denomination is a different asset (an IBC voucher or a factory token). Pricing it as ATOM would overstate your income. The receipt is kept with its real denomination until you say what it is.",
		Suggested: "Valueless if it is an unsolicited lookalike; Override with the real token and its price if you know what it is.",
	},
	{
		Code:      ReasonUnverifiedFactory,
		Title:     "Issuer-created token",
		Text:      "This is an issuer-created Juno tokenfactory asset, not native ATOM. No native ATOM backing or reliable market price has been verified.",
		Explainer: "Anyone can mint a tokenfactory asset and name it after a real coin. This one resolved to a Juno factory denomination with no verified market, so no price is applied.",
		Suggested: "Valueless, unless you hold it deliberately and can price it.",
	},
	{
		Code:      ReasonIdentityMismatch,
		Title:     "Token identity unresolved",
		Text:      "",
		Explainer: "The token's identity could not be resolved from the Hub's registries, so its name and price are unknown. It may be a legitimate asset from another chain that we simply cannot price, or an unsolicited one.",
		Suggested: "Override with the token and its price if you know them; Valueless if you did not ask for it and it has no market.",
	},
}

var reviewReasonByCode = func() map[string]ReviewReason {
	out := map[string]ReviewReason{}
	for _, r := range reviewReasons {
		out[r.Code] = r
	}
	return out
}()

// ReviewReasonGlossary returns the reason catalog for API responses.
func ReviewReasonGlossary() []ReviewReason { return reviewReasons }

// addReason records a reason code and, when the reason has display text,
// the text in Reasons. Codes without text (identity_mismatch) leave Reasons
// untouched so record fingerprints computed before codes existed still match.
func (a *AssetIdentity) addReason(code string) {
	for _, existing := range a.ReasonCodes {
		if existing == code {
			return
		}
	}
	a.ReasonCodes = append(a.ReasonCodes, code)
	if text := reviewReasonByCode[code].Text; text != "" {
		a.Reasons = append(a.Reasons, text)
	}
}
