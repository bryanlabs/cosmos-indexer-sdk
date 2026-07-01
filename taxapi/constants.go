package taxapi

// Row.Category and Row.Direction literals, shared across export, 8949 and row
// building. The tax layer's classifications (tax.Category*) marshal to the same
// strings; fee rows are built from the SDK's generic Fee table at export time,
// so "fee" exists only on this side.
const (
	categoryReward     = "reward"
	categoryCommission = "commission"
	categoryFee        = "fee"
	categoryNFTSale    = "nft_sale"
	categoryNFTMint    = "nft_mint"
	categorySwap       = "swap"

	directionIn  = "in"
	directionOut = "out"
)
