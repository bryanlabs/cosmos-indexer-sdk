package taxapi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// The commission page serves each validator's newest accepted versioned
// result and falls back to the legacy run table for validators that have
// none. A versioned result is accepted only when it comes from one of the
// reviewed collections below, carries that collection's exact build, private
// profile and window, names the validator it is stored under, and finished:
// ok, reconciled or drift. A stored error is not a result. A drift result is
// a finished trace whose rebuilt balances differ from the chain; its own page
// shows it labelled as drift and the Hub leaves it out of its totals.
//
// The API reads only the identity, run and rows columns. Evidence stays
// private and is never selected.

// commissionServingSource is one versioned collection the API may serve.
// Root, when set, limits a retry collection to the one validator whose
// sha256 it pins.
type commissionServingSource struct {
	Version string
	Build   string
	Profile string
	FromDay string
	AsOf    string
	Root    string
}

const (
	flowCorrectedBuild   = "sha256:fc277f445a4257a8855bce376c44cc71cc90a51417f72be8cba5e141d0abafd4"
	flowCorrectedProfile = "6bab7886b14ec3728c98f7e9b5e77a9b2f422a5d0fbc782178e8be65586739ce"
	flowCorrectedPrefix  = "ops818-page82a-profile53da2904e3a6-sourcesade534f8e5f7-"
	flowCorrectedFrom    = "2024-03-21"
	flowCorrectedAsOf    = "2026-11-12"
)

func flowCorrected(suffix string) commissionServingSource {
	return commissionServingSource{Version: flowCorrectedPrefix + suffix, Build: flowCorrectedBuild, Profile: flowCorrectedProfile, FromDay: flowCorrectedFrom, AsOf: flowCorrectedAsOf}
}

// flowCorrectedRoot is a corrected-build retry that may serve only its one
// pinned validator root.
func flowCorrectedRoot(suffix, root string) commissionServingSource {
	r := flowCorrected(suffix)
	r.Root = root
	return r
}

// The LSM reward retries run the native build that reads the corrections
// manifest without the six tokenize-share rewards, after hub4 heights
// 31944427 and 31944434 were re-indexed to store those rewards themselves.
// Their stored profile binds that manifest, so build, profile and version
// prefix all differ from the corrected shards.
const (
	flowLSMBuild   = "sha256:b7075f6a5b020616d7ad4b6b8f1275e6a699f10a30ff148dde7e800e4e0e56b8"
	flowLSMProfile = "dc197eef1ed664d2f53b2453a3355ac97cd052f827b2bf117cd180db25dbba68"
	flowLSMPrefix  = "ops818-page82a-profile53da2904e3a6-sources40d696172138-"
)

// flowLSMRoot is an LSM reward retry that may serve only its one pinned
// validator root.
func flowLSMRoot(suffix, root string) commissionServingSource {
	return commissionServingSource{Version: flowLSMPrefix + suffix, Build: flowLSMBuild, Profile: flowLSMProfile, FromDay: flowCorrectedFrom, AsOf: flowCorrectedAsOf, Root: root}
}

// The giant reruns stop tracing at a wallet that sent ATOM to more than
// 50,000 receivers. They share the corrected profile, window and version
// prefix but run their own build. Most entries serve a validator whose run
// never reached such a wallet, so its result is the one the corrected build
// computes. The rest reached one, never finished on the corrected build, and
// booked under 0.1% of their commission to wide_distributor, which bounds
// what the stop can change.
const flowGiantBuild = "sha256:324924bc987eeaa3d0475a9c0bda70cddf718027c421c890ca90663389eeba46"

func flowGiantRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowGiantBuild
	return r
}

// The withdraw-address reruns fix reward rows that credited the delegator
// when the chain paid a different withdraw address. They keep the corrected
// profile, window and version prefix, run their own build, and each serves
// only the validator whose rerun passed acceptance against its served row.
const flowWithdrawFixBuild = "sha256:ae39819c1af339987de665826c35edb317e6ad11b5390d3d7e9a5476228c8e80"

func flowWithdrawFixRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowWithdrawFixBuild
	return r
}

// The slash reruns run the build that burns validator slashes from the
// traced stake and reads refund and return-credit attributes per event. They
// keep the corrected profile and version prefix under their own build.
const flowSlashBuild = "sha256:2dba2a3fbd1f96304c302313d45b624de8572b7174039f79046a14f37ecef8f9"

func flowSlashRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowSlashBuild
	return r
}

// The client-trace reruns run the slash build plus the IBC refund pairing
// that keeps a Skip Go send's client trace (sdk 7320ae4).
const flowClientTraceBuild = "sha256:9b1737b7e47ac4e402b583e58d8aa45992d821d34a389c6a98300efd2c0053c7"

func flowClientTraceRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowClientTraceBuild
	return r
}

// The self-bond reruns run the client-trace build after the self-bond
// delegate rows from MsgCreateValidator were added to the ledger (sdk 4e7f529).
const flowSelfBondBuild = "sha256:7f041d0a3acb08f01bb97ef92c6da88d49770285e5f6c7cd013e08937bdf9e44"

func flowSelfBondRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowSelfBondBuild
	return r
}

// The tokenize-share reward reruns run the self-bond build after a
// tokenize-share record's reward payouts were counted in the trace (sdk 2d51444).
const flowTokenizeRewardBuild = "sha256:613de71fb129fa7a3aaf1c6d8cc2f72df98ae5be6cc0ffa459703323b95dfa85"

func flowTokenizeRewardRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowTokenizeRewardBuild
	return r
}

// The all-fixes reruns run one build with every trace fix so far: withdraw-address
// moves, slash burns, client-trace refunds, self-bond delegations, nft_sale
// proceeds, tokenize-share record rewards, the wasm fallback, and withdraw-address
// moves left out of the delegator (sdk cb34569).
const flowAllFixesBuild = "sha256:4c81b7cc1448c2a8a117d39f3fae7b3d8645d50a563a66caafdc3fbfb3c48151"

func flowAllFixesRoot(suffix, root string) commissionServingSource {
	r := flowCorrectedRoot(suffix, root)
	r.Build = flowAllFixesBuild
	return r
}

// flowLSMClientTraceRoot is a client-trace rerun of a validator whose served
// row came from the LSM reward retry, so it keeps that retry's profile and
// version prefix.
func flowLSMClientTraceRoot(suffix, root string) commissionServingSource {
	r := flowLSMRoot(suffix, root)
	r.Build = flowClientTraceBuild
	return r
}

// defaultCommissionServingSources is in priority order: registered retries
// first, then the corrected shards, then the two source collections the
// shards adopt from. A validator is served from the first collection that
// holds an accepted result for it.
var defaultCommissionServingSources = map[string][]commissionServingSource{
	apiMainnet: {
		flowAllFixesRoot("accel-withdraw-fix-131a106eafdd-20261118-fanout-v1", "131a106eafddf1bb1a3228ebbbe88803d3024c87e82bf2d80d27af58e563eb5c"),
		flowAllFixesRoot("accel-withdraw-fix-ed329a6a4108-20261118-fanout-v1", "ed329a6a41088912b52d8d74d524b852d6990bbb039b93ceda82f349ed3155c6"),
		flowAllFixesRoot("accel-withdraw-fix-cb45206dc2fe-20261118-fanout-v1", "cb45206dc2fe6ff60090ef898765e02de8ae1242cd158b19c466694b84829b8d"),
		flowAllFixesRoot("accel-withdraw-fix-cab01accf0bc-20261118-fanout-v1", "cab01accf0bc0d466f3d0054f3c02163b2184d0bacaab7ba35007b71d337a488"),
		flowAllFixesRoot("accel-withdraw-fix-5ddd3d9c5bcb-20261118-fanout-v1", "5ddd3d9c5bcb5ba1dc12461d702f5acf923b470e49be328ab7c2e6fab813428f"),
		flowAllFixesRoot("accel-withdraw-fix-c367934baa77-20261118-fanout-v1", "c367934baa77ab0ce85a84cbd4ec158dfcfdac3f0df7e6efdbf5d02eaff5938b"),
		flowSlashRoot("accel-withdraw-fix-52836aab8ac6-20261118-fanout-v3", "52836aab8ac6250910ea4e77539c7ef6ef8fa98050b057271b80919434a13e42"),
		flowSlashRoot("accel-withdraw-fix-0a11a4c00904-20261118-fanout-v3", "0a11a4c0090436746a12555ec8f07a503c467e849bccf9ef550a8e2985f2b5ab"),
		flowSlashRoot("accel-withdraw-fix-d2d7b3c161a2-20261118-fanout-v3", "d2d7b3c161a280621a0b05189e3746f1d785eab94cc4de6096a76a621c80d2b5"),
		flowAllFixesRoot("accel-withdraw-fix-001308de5051-20261118-fanout-v7", "001308de505138aa91ea460b148382f1dd7b6528a4a307e6333dff716f08a7ae"),
		flowAllFixesRoot("accel-withdraw-fix-ab12977c1a21-20261118-fanout-v7", "ab12977c1a2135f7aeea69534171ee3c2bec9a03f3650cbf5dff76caefb19345"),
		flowSlashRoot("accel-withdraw-fix-a5b85d99e09a-20261118-fanout-v3", "a5b85d99e09a1b9844bbb4209119b7b1d730b83b37c8e45bae11e21c09428965"),
		flowWithdrawFixRoot("accel-withdraw-fix-06d2a6fb6513-20261118-fanout-v1", "06d2a6fb65137dc2b6730a227548da0ab3d738cc6bf2a561c68ed9154c293c1b"),
		flowSlashRoot("accel-withdraw-fix-32608eb7f728-20261118-fanout-v3", "32608eb7f7286cab627246cc922e7b874857acf5a5dce90ebff5d5623f184eb2"),
		flowSlashRoot("accel-withdraw-fix-0d96280b5e9a-20261118-fanout-v3", "0d96280b5e9a7d7e720633d37a252722db055189337b778f1ae5d805a80eab84"),
		// 1456c0b64ef4 counts its tokenize-share record's reward payouts: drift
		// 0.12 ATOM on 7,514 ATOM, overdraw 0.000036, shares unchanged.
		flowTokenizeRewardRoot("accel-withdraw-fix-1456c0b64ef4-20261118-fanout-v5", "1456c0b64ef40bd1073983d272a32eb28333805b6470a6903d4f776ec2eeb9fc"),
		flowSlashRoot("accel-withdraw-fix-6381eb3d4d63-20261118-fanout-v3", "6381eb3d4d63ccfe34abfde32b00c5692b94fb1081ba8628fb55a32d9b822201"),
		flowSlashRoot("accel-withdraw-fix-7745d8ffeac6-20261118-fanout-v3", "7745d8ffeac67f746b31f6cbdc249829bb77eb1708c12f798a9dd9bb45ba9ce1"),
		flowSlashRoot("accel-withdraw-fix-17fa80ffb90c-20261118-fanout-v3", "17fa80ffb90c55459967e90d18555042a950e8b95b05f19ff2f922da458baa7e"),
		flowSlashRoot("accel-withdraw-fix-2453c25e0fd9-20261118-fanout-v3", "2453c25e0fd99d77519b1f1ddc9805b827da1c1df87bc099d511a3626065633a"),
		flowAllFixesRoot("accel-withdraw-fix-f54f5e725ac7-20261118-fanout-v7", "f54f5e725ac75b7c1be4fcaee9d2c5ac24fb41b08fb7f8838dca9fb79580ee74"),
		flowSlashRoot("accel-withdraw-fix-b6de900ea24b-20261118-fanout-v1", "b6de900ea24b3ed88a9db14daf000adb1a2f52334f946166f6ad096c77c68548"),
		// A labelled drift row: Hub marketplace bid escrow has no rows yet, and the row it replaces drifted 208,882 ATOM.
		flowAllFixesRoot("accel-withdraw-fix-6fb852133855-20261118-fanout-v7", "6fb8521338557f53f633e24360bb5b102079730968e7b81d2f58fbfbb3b54cb0"),
		// A labelled drift row: its rerun drifts 237 ATOM, and the row it replaces drifted 208,278 ATOM.
		flowSlashRoot("accel-withdraw-fix-f1afab0130fb-20261118-fanout-v3", "f1afab0130fb05642a485af36968c826238e912292f4616ac0658957fcfb28d1"),
		flowSlashRoot("accel-withdraw-fix-a3e91bffa7c0-20261118-fanout-v3", "a3e91bffa7c05edeb6836f0d37d7bd8d8ed18788e8dd4af9b806caa5d7b2de71"),
		flowSlashRoot("accel-withdraw-fix-c07d4e7c6b6e-20261118-fanout-v3", "c07d4e7c6b6e288cc7c23111d04eca5c50872c5b58fdb9b5f2ec67ae2093cdad"),
		flowSlashRoot("accel-withdraw-fix-d2c321cc8c4e-20261118-fanout-v3", "d2c321cc8c4ec629077ecc8e006f372bc361c918ef0fcb540372fd347746cbcc"),
		flowSlashRoot("accel-withdraw-fix-8df708d2db2f-20261118-fanout-v3", "8df708d2db2f5583431b8f9ce8a6e3ba9ae7840e0f78b38366948a69c0d24716"),
		// Self-bond rerun: its own self-bond had no ledger row, so the old trace kept the bond liquid; about 0.79 ATOM moves to ibc_out, 0.38 from restaked_own and 0.41 from held (66.0 pct class share).
		flowSelfBondRoot("accel-withdraw-fix-1e6691e3985a-20261118-fanout-v5", "1e6691e3985a8f008990dbebf047b578651e022a4f608e7f48b939f571d4e3ab"),
		flowSlashRoot("accel-withdraw-fix-fda70bc2ca4c-20261118-fanout-v3", "fda70bc2ca4ce5311f719f17a2e26c8339c2e727ce3ad9309ecf27f8c039e02b"),
		flowSlashRoot("accel-withdraw-fix-7f2f32dfb1d5-20261118-fanout-v3", "7f2f32dfb1d5d391d70b85d967e90ae248d7aa4dd109656f633a893c98febea6"),
		flowAllFixesRoot("accel-withdraw-fix-75f92cb68a7f-20261118-fanout-v7", "75f92cb68a7f639c979be19a1d7ca4b7ce6ad63a2341562f3c966f82c7ab920d"),
		flowSlashRoot("accel-withdraw-fix-f467f62240db-20261118-fanout-v3", "f467f62240db0faad0c3394d0f0be34ec1ed8a1310d287c04cd46b648525b571"),
		// Its rows are over the inline limit and served from verified chunks.
		flowClientTraceRoot("accel-withdraw-fix-b970df6089e6-20261118-fanout-v4", "b970df6089e634f8cd9c6b5a4fff54656cb34cbb9d0cf568611e963d313dccdb"),
		flowSlashRoot("accel-withdraw-fix-bf9b46882519-20261118-fanout-v3", "bf9b46882519b68cecbc4399741d4c85064fcbfa3fd555333dda5525f12f0a93"),
		flowWithdrawFixRoot("accel-withdraw-fix-007bd61b16ac-20261118-fanout-v1", "007bd61b16ac3792987956d92c01c689478dac00d39979d0e8acb010a07c50b3"),
		flowSlashRoot("accel-withdraw-fix-0da7be31faa8-20261118-fanout-v3", "0da7be31faa899b2a6ed0dcfb7fcdec0b2811c102eebbfc106027bab2f54d46c"),
		flowSlashRoot("accel-withdraw-fix-0a69bace753e-20261118-fanout-v3", "0a69bace753e236747f228b9b9cd1bf54e7115f8bee6015e666002b4c46c9e84"),
		flowSlashRoot("accel-withdraw-fix-a3c4f7ccef86-20261118-fanout-v3", "a3c4f7ccef864eaee6395a776fdf14b7c0b755ed21f49e9c89cfda0447dba7a1"),
		flowSelfBondRoot("accel-withdraw-fix-65707e97ab7c-20261118-fanout-v5", "65707e97ab7c15f24279c6b0c181bcebddc72e7f8580bad4cbfcf189dbea9d7e"),
		flowAllFixesRoot("accel-withdraw-fix-09450e35c400-20261118-fanout-v7", "09450e35c4006cbfbd842e08cc4791a2cdbab96a88654777cc9a040297d80c9b"),
		flowSlashRoot("accel-withdraw-fix-84ac2be58662-20261118-fanout-v1", "84ac2be58662525df11258ecf1a826044fdaeb1f82b0a4e3e85a806f409a3fa0"),
		flowSlashRoot("accel-withdraw-fix-00a65ed900f9-20261118-fanout-v1", "00a65ed900f97cecf840ff2051c08e7a1c384fe07f648ce2099f425b9c579fdf"),
		flowSlashRoot("accel-withdraw-fix-728154820784-20261118-fanout-v1", "7281548207848f0d0f7de142b10be3830a4be0450cb51603ef323d5fa90d621f"),
		flowSlashRoot("accel-withdraw-fix-f8bad4552c9c-20261118-fanout-v1", "f8bad4552c9c01b57663e31de2536595c1a5f36d4f818810c99b7579a0442d34"),
		flowAllFixesRoot("accel-withdraw-fix-529022bbcad4-20261118-fanout-v7", "529022bbcad4b4fda219945769eaa658cbc9be549857319d1893a7282245bc00"),
		flowSlashRoot("accel-withdraw-fix-6adc774f56d4-20261118-fanout-v1", "6adc774f56d4677f42461e9b5a72107ec9b4acb4d7921aa979aebe634b456c48"),
		flowSlashRoot("accel-withdraw-fix-6e0c83d2796a-20261118-fanout-v1", "6e0c83d2796a5a3e5401fcf4e6d7dbc85c637bd803ffa4d6e38b0200a23c48d1"),
		flowSlashRoot("accel-withdraw-fix-2b251978865c-20261118-fanout-v1", "2b251978865c4ae3262c2e1c1bd2232fb3c1a6a32ae2aee649daebb5ae99fe45"),
		flowSlashRoot("accel-withdraw-fix-334894d36534-20261118-fanout-v1", "334894d36534f55b36282d9dcd822d719c62db3e3d9af320a27ec4ffaa8bb5a7"),
		flowAllFixesRoot("accel-withdraw-fix-53857068a2c7-20261118-fanout-v7", "53857068a2c766dfaeb9149bbef2f02eebcac0f091d4dd326a03b90cf73a3bd3"),
		flowSlashRoot("accel-withdraw-fix-583d96ba1bba-20261118-fanout-v1", "583d96ba1bba3be225003af20b7cbad0899e6e8b98c12857d2dfbb2df7cdadf5"),
		flowSlashRoot("accel-withdraw-fix-8a3aea50be11-20261118-fanout-v1", "8a3aea50be11ad5d460bc6e832b2ea5eb6e0774f75b9e3dda984158ce01b35c6"),
		flowTokenizeRewardRoot("accel-withdraw-fix-ce871c7c6e35-20261118-fanout-v5", "ce871c7c6e35d0f1629d9c44ffe8500872e58f6bca1e188223fe4e4835fdb062"),
		flowAllFixesRoot("accel-withdraw-fix-06c9061fd494-20261118-fanout-v7", "06c9061fd4948af545ba753349029fc8c6a5de2dc1d2f725ca4531058177f714"),
		flowSelfBondRoot("accel-withdraw-fix-0f53f6fc13d9-20261118-fanout-v5", "0f53f6fc13d9f7022c8ee7a0aa661c0440ea6027ff4e934e7952601cfb526e2c"),
		flowSlashRoot("accel-withdraw-fix-162c315eab1a-20261118-fanout-v1", "162c315eab1a4d9544efe0fc8a5b3ad847f1381a728a99913751921ce365c019"),
		flowAllFixesRoot("accel-withdraw-fix-28197c71f63d-20261118-fanout-v7", "28197c71f63d1583b31d7cc881b76841d4474815168c3e63e53ab527bb25b785"),
		flowAllFixesRoot("accel-withdraw-fix-70bb65c3bc87-20261118-fanout-v7", "70bb65c3bc87785d35081cbd0435a18962ef4eac525d0b54a0580907dc509737"),
		flowSlashRoot("accel-withdraw-fix-8b22b9cdcee2-20261118-fanout-v1", "8b22b9cdcee26d056002e8efb2fcfad7a77c828f7b3a67661af9bbb6c14886e6"),
		flowSlashRoot("accel-withdraw-fix-8f1c190dc60a-20261118-fanout-v1", "8f1c190dc60aa0c8acbfe877bd5dbeb274e68258b452908cc76eedd5adc4435c"),
		flowAllFixesRoot("accel-withdraw-fix-940ef932a5d6-20261118-fanout-v7", "940ef932a5d68cf9104dfd219b2230285fb3b1f06721b9fd7fb2283a8500b03d"),
		flowSlashRoot("accel-withdraw-fix-b384bc959cad-20261118-fanout-v1", "b384bc959cad3296e74490e5fc7b9ad7e93e34593658e3276949d0db16096ecb"),
		flowSlashRoot("accel-withdraw-fix-b5701732e313-20261118-fanout-v1", "b5701732e3137a2d9e86e4bde847fcbb6329c98481658c574c145f1a3811fb03"),
		flowSlashRoot("accel-withdraw-fix-d4c6ccac7396-20261118-fanout-v1", "d4c6ccac73962d84cd3d4cec2c70851d0ea02940ce0436882450532c2df4f9e6"),
		flowSlashRoot("accel-withdraw-fix-f009da4d5b44-20261118-fanout-v1", "f009da4d5b4452af83e81335ad2d4a2db70d9d7f8d226b30f76e4c8b5bcf5da8"),
		flowTokenizeRewardRoot("accel-withdraw-fix-f76e659775a6-20261118-fanout-v5", "f76e659775a647b387ed8fc2c4f49bf63e6f5d1c4756e3391863764cf6fb01c9"),
		flowSlashRoot("accel-withdraw-fix-139a197e4cda-20261118-fanout-v1", "139a197e4cdaab48661383f71730eb1d481d25daf8906c311e7ad0294dc61649"),
		flowSlashRoot("accel-withdraw-fix-39722f592341-20261118-fanout-v1", "39722f59234103dc29a8caaf03d6c1622b6954fb01f6b083bcd9106b2d7249f4"),
		flowSlashRoot("accel-withdraw-fix-7f2e4c9d47f3-20261118-fanout-v1", "7f2e4c9d47f315d654fc6fa6a53fa0d908a5cf4ac432d5bee73708508e28ad8f"),
		flowSlashRoot("accel-withdraw-fix-317097d52dd7-20261118-fanout-v1", "317097d52dd71d08f60d71a3622af4d4a442ebd012cd4e99d98367746de5b17d"),
		flowSlashRoot("accel-withdraw-fix-d5f91bf8d7b4-20261118-fanout-v1", "d5f91bf8d7b41fe125a39816d950a6d840bc9cc52b412eef07bee698f0b9e182"),
		flowSlashRoot("accel-withdraw-fix-383d9fd329b2-20261118-fanout-v1", "383d9fd329b21ef289a8bc584e465325e26025d5b1dd35b74811bf1d831e4afe"),
		flowClientTraceRoot("accel-withdraw-fix-d16f03fef762-20261118-fanout-v1", "d16f03fef76269e52ea1c39dc50847ccc561d1e89782366372bc13f936a0c525"),
		flowClientTraceRoot("accel-withdraw-fix-e19812c884e7-20261118-fanout-v1", "e19812c884e754f7c64a4cc29e18fd0af0a0f06dd717d82edaa3800e4185b4a8"),
		flowClientTraceRoot("accel-withdraw-fix-e7790b332148-20261118-fanout-v1", "e7790b332148a7573e8bad81ee2a5242f491435aec72cdc72eaee7b25c298565"),
		flowClientTraceRoot("accel-withdraw-fix-8a571bb0887f-20261118-fanout-v1", "8a571bb0887f0667fc25cac41c2d25c7d2c27a636f639d51f7f102b62969b172"),
		flowClientTraceRoot("accel-withdraw-fix-ad49f408bd75-20261118-fanout-v1", "ad49f408bd756cae1952d8f85f04d4210a75a1b20797e03412fd8b95a23f4938"),
		flowClientTraceRoot("accel-withdraw-fix-1f92328f06d6-20261118-fanout-v1", "1f92328f06d661012c9b0196cac76bb8168b1370009fc5ffe934dd63659d0b61"),
		flowSelfBondRoot("accel-withdraw-fix-3e3481ef24f7-20261118-fanout-v5", "3e3481ef24f791529e1ff1bf325d48d3ecccf38248bc9ef24ca623890e0d4b45"),
		flowClientTraceRoot("accel-withdraw-fix-a149b3077b86-20261118-fanout-v1", "a149b3077b86222dfc773827c1c37a42c4755339a0c1520d344aa4615385cd38"),
		flowAllFixesRoot("accel-withdraw-fix-5fa7cab34d20-20261118-fanout-v7", "5fa7cab34d20f2a77714dae2730dc0286ed77207a35099cf6d8128b08f375af1"),
		// Self-bond rerun: its own self-bond had no ledger row, so the old trace counted the bond as liquid and matched the exchange transfers to it; all 2.41 ATOM moves from held to exchange_like under fifo, overdraw 2.8 to 0.000001.
		flowSelfBondRoot("accel-withdraw-fix-b3bcdd688853-20261118-fanout-v5", "b3bcdd6888530414e97414c962742b3c4d8b56b02e6d80b152dafa9e5915a5d7"),
		flowSelfBondRoot("accel-withdraw-fix-4fcff5fcb5a5-20261118-fanout-v5", "4fcff5fcb5a55ce3e56d6c89bd59739729a349559f0374be1ccb0b825e36747c"),
		flowClientTraceRoot("accel-withdraw-fix-d3bfac7ecf84-20261118-fanout-v1", "d3bfac7ecf84f17408ba216adde4b3c6969adad03e14dacf0290d287d107fea5"),
		flowClientTraceRoot("accel-withdraw-fix-95ceecf77830-20261118-fanout-v4", "95ceecf77830b8642cd1e669655f504c4d990eb528552e5b3a272c091b78428a"),
		flowSlashRoot("accel-withdraw-fix-b03cdf1c7313-20261118-fanout-v1", "b03cdf1c7313b261dcf5e40862ed7ff148b7654aa5d8bb244d21e8d1beef365b"),
		flowClientTraceRoot("accel-withdraw-fix-8837e708a0bd-20261118-fanout-v1", "8837e708a0bd1c91f7d7267df9de922b07b297a01abd041f07d5b6ce797858ae"),
		flowClientTraceRoot("accel-withdraw-fix-e448dfbf05be-20261118-fanout-v4", "e448dfbf05be26541b5fa7bcea3d85dcac6e5356759d89dadf4f9bd3c8b182d1"),
		// 4643e858df25 drifts 15.7 ATOM more than its old row, inside tolerance,
		// with overdraw down 282 ATOM and the slash burn and IBC refunds added.
		flowClientTraceRoot("accel-withdraw-fix-4643e858df25-20261118-fanout-v1", "4643e858df25fd68d90a19ca3eb4f2a0a9c8c6cb604f2e5c92c284d5af480d62"),
		flowClientTraceRoot("accel-withdraw-fix-2d473f4858d9-20261118-fanout-v4", "2d473f4858d9987775254ce4bff8939f812bfefeb04eb1e6f0a5ebde72969fd8"),
		flowClientTraceRoot("accel-withdraw-fix-021b3d76a3a7-20261118-fanout-v1", "021b3d76a3a7b812b345102cb2856b74e57e6b5dae2ddd2eb53df30f8e550b0d"),
		flowClientTraceRoot("accel-withdraw-fix-928644401359-20261118-fanout-v1", "9286444013590806985c200f2860b383a1aea12df40a1d155a8b2be7a75da37f"),
		flowClientTraceRoot("accel-withdraw-fix-65858093b487-20261118-fanout-v1", "65858093b487eb21286e785487ff7bafdda9a0effdbd492296a3ec88adac99a7"),
		flowClientTraceRoot("accel-withdraw-fix-ea3690afb084-20261118-fanout-v1", "ea3690afb084f65e0da1d03c2f8f831d8be946809c3c09422b81afd76d9427a0"),
		flowClientTraceRoot("accel-withdraw-fix-587d6d7bc979-20261118-fanout-v1", "587d6d7bc97969380199e8f9038208fc8611da31a6e141415780e8c2604b34bf"),
		flowClientTraceRoot("accel-withdraw-fix-519012b501e4-20261118-fanout-v1", "519012b501e410adc9cb826c027efedc7fd0ccd9b5e89d27f2c3712dabc38ed3"),
		flowClientTraceRoot("accel-withdraw-fix-9145229fa3e1-20261118-fanout-v1", "9145229fa3e16e5795ff15a828cc8daf130219f4bca59de0f63f412cc5f6505b"),
		flowSelfBondRoot("accel-withdraw-fix-d712be52b0fb-20261118-fanout-v5", "d712be52b0fbdb6fcef53d5596b729b89e70e0d40afbff5d01f4cbe64d7631f4"),
		flowAllFixesRoot("accel-withdraw-fix-67c1c59d6b78-20261118-fanout-v7", "67c1c59d6b78f4b099a5ed5d164e4158f99c8b57b99c16463845bb6d79a69316"),
		flowClientTraceRoot("accel-withdraw-fix-faac294cdafc-20261118-fanout-v1", "faac294cdafc8860e4e0adfce65714b0100ecbfce8c8af9e14329ca9fa415219"),
		flowClientTraceRoot("accel-withdraw-fix-5187c6ad32a6-20261118-fanout-v1", "5187c6ad32a6209ebd2f8b36e2f78b95950a96e046c474db04ca64b8c981f885"),
		flowAllFixesRoot("accel-withdraw-fix-1663fc05d7e2-20261118-fanout-v7", "1663fc05d7e2e2427b2063fdb264eda51e956e65e869efaa0a2f01c09971a12d"),
		flowSlashRoot("accel-withdraw-fix-0af248dd5028-20261118-fanout-v1", "0af248dd5028935d07fa57bc8e3c5a88779c2119a479ea67fed440773020ca7b"),
		flowClientTraceRoot("accel-withdraw-fix-7b7d4777c730-20261118-fanout-v1", "7b7d4777c730c08531bd3809dfec240e8d317e7f73ff324ea09713901a95b150"),
		flowAllFixesRoot("accel-withdraw-fix-256067afb286-20261118-fanout-v7", "256067afb286cf77cbcd49c186eede037db3eb840d31dbe8d56b6fc6fc4faaa6"),
		flowClientTraceRoot("accel-withdraw-fix-6ef922ee6733-20261118-fanout-v1", "6ef922ee6733c1be94bb847fe1e62b889d94aed3fa33e8fe2a685a4ff08b6976"),
		// 91442d368a45 drifts 57.8 ATOM more than its old row, inside tolerance,
		// with wallets 15 to 488, overdraw down 8892 ATOM and the slash burn added.
		flowClientTraceRoot("accel-withdraw-fix-91442d368a45-20261118-fanout-v1", "91442d368a45f15d2e40ce0b0a990d388e45a8fb46490d28877d52f5260af21c"),
		flowClientTraceRoot("accel-withdraw-fix-f7e8484fc529-20261118-fanout-v1", "f7e8484fc5297478aa1f1746f02448ceff54d07dab8341ec4dde189ee4044b26"),
		// 3c4cbfc796a3 has overdraw down 5000 ATOM, and 4567 ATOM moves from
		// ibc_out to exchange_like after the withdraw-address fix.
		flowClientTraceRoot("accel-withdraw-fix-3c4cbfc796a3-20261118-fanout-v1", "3c4cbfc796a3df578d91831a756af2097d9c3d09e2f26695df1f483eff8c87d0"),
		// 3e8353a25770 drifts 0.23 ATOM more than its old row, inside tolerance,
		// with shares unchanged; served for the corrected build.
		flowClientTraceRoot("accel-withdraw-fix-3e8353a25770-20261118-fanout-v1", "3e8353a2577021f282d5233fa3ad024dd98f6e12d14e038e9d50f3daca85eca1"),
		flowClientTraceRoot("accel-withdraw-fix-c094ace10c53-20261118-fanout-v1", "c094ace10c53d5e89062832b81b99265a5b884024b90f47fae9bfd05611f7901"),
		// a54eb3d66496 counts its tokenize-share record's reward payouts: drift
		// 0.30 ATOM on 220,610 ATOM with no overdraw, shares unchanged.
		flowTokenizeRewardRoot("accel-withdraw-fix-a54eb3d66496-20261118-fanout-v5", "a54eb3d66496370f607e64ffb3b1ff1ea939901a626300de624373ab8b18c9b9"),
		flowClientTraceRoot("accel-withdraw-fix-e0c9d081b175-20261118-fanout-v1", "e0c9d081b175113666052f96fcaae1f38b803abc3fdcd7618fd05cc1f13e7210"),
		flowClientTraceRoot("accel-withdraw-fix-debf605ff680-20261118-fanout-v1", "debf605ff68022a7b96ba4f05f55025383b8d76f7198338d672c30b406a10685"),
		flowClientTraceRoot("accel-withdraw-fix-359f0bf28de5-20261118-fanout-v1", "359f0bf28de54aea4eb43e612f91fd001b794c1e292e0506fecfc1eed6ebcdfb"),
		flowClientTraceRoot("accel-withdraw-fix-51535f062e5b-20261118-fanout-v1", "51535f062e5bebbc70e87b5e170ea1485c5fa5602e1c1728b3e2e42f4f84cdc9"),
		// Self-bond rerun: its own self-bond had no ledger row, so the old trace kept the bond liquid; about 0.40 ATOM moves from held to ibc_out and fees (40.1 pct class share).
		flowSelfBondRoot("accel-withdraw-fix-5cc8184acf42-20261118-fanout-v5", "5cc8184acf429429e5835191771de09f571f5ec8249905ca60ae6ea2affdf673"),
		flowClientTraceRoot("accel-withdraw-fix-cb4a1161be29-20261118-fanout-v1", "cb4a1161be29b103e59b070f3bae6136327428f746a3409d1dc249d623533a53"),
		flowSelfBondRoot("accel-withdraw-fix-a52384ba0f34-20261118-fanout-v5", "a52384ba0f34c8c6df7d911bd8673ed75e44af0d41e4dffbec3358f03b015ae8"),
		flowClientTraceRoot("accel-withdraw-fix-48847a216b03-20261118-fanout-v1", "48847a216b03b20746514b1858d1f3c6733d4803bfba939e550af7170765d214"),
		flowClientTraceRoot("accel-withdraw-fix-0acf3614937b-20261118-fanout-v1", "0acf3614937b874374e73143530c23d6c2057a5840f255ea7bc0f1186bd39b46"),
		flowClientTraceRoot("accel-withdraw-fix-d73f8ec0ba97-20261118-fanout-v1", "d73f8ec0ba9793004860382e2b2bb58eda4db08a36d728b2a8756776f3678815"),
		flowClientTraceRoot("accel-withdraw-fix-2f69743a8941-20261118-fanout-v1", "2f69743a8941faee5918fcbdf46ea6057a376d1902856bb094f29c411aaca3b5"),
		flowClientTraceRoot("accel-withdraw-fix-bf186106324a-20261118-fanout-v1", "bf186106324a43d5f5f1a50226587f721047064e54ec1730546bdf40701061de"),
		flowClientTraceRoot("accel-withdraw-fix-0557c428ef4c-20261118-fanout-v1", "0557c428ef4cac55c6a9f6487054ccf620dbb6dec10c3627b9124a3b3b8e31ad"),
		flowClientTraceRoot("accel-withdraw-fix-802b450f0236-20261118-fanout-v1", "802b450f0236afca7078a0ce916d993557ed8d48131e801f7262c02da500a807"),
		flowClientTraceRoot("accel-withdraw-fix-892530d89fce-20261118-fanout-v1", "892530d89fce820e8ce6005046a8a93340487899500d6f795c82d0080d11d3ff"),
		// Self-bond rerun: its own self-bond had no ledger row, so the old trace kept the bond liquid; about 0.145 ATOM moves from held to restaked_own (28.3 pct class share).
		flowSelfBondRoot("accel-withdraw-fix-e9dd7b1d8994-20261118-fanout-v5", "e9dd7b1d8994732d3ddf71250574ced87a7287b9032f76c9b6f5cfdcccc093e4"),
		flowClientTraceRoot("accel-withdraw-fix-3dd844297fbd-20261118-fanout-v1", "3dd844297fbda1ffe59720f953e2168064237e3436b5d3a81936d275496ffecf"),
		flowClientTraceRoot("accel-withdraw-fix-28c5d00f4530-20261118-fanout-v1", "28c5d00f45307b23be175cab0095c64c8ca2dd806e6895fe1cee42456948213e"),
		// Self-bond rerun: its own self-bond had no ledger row, so the old trace kept the bond liquid; about 1.0 ATOM moves from held to restaked_own (25.6 pct class share).
		flowSelfBondRoot("accel-withdraw-fix-2877ad667da5-20261118-fanout-v5", "2877ad667da5260e6188746799121924cb3fd1e13043450dad6192ec7387803a"),
		// Self-bond rerun: its own self-bond had no ledger row, so the old trace kept the bond liquid; all 0.50 ATOM moves from held, mostly to restaked_own (100 pct class share).
		flowSelfBondRoot("accel-withdraw-fix-651b835c3d53-20261118-fanout-v5", "651b835c3d5335dc4604c3e1ba8c43cf83b245160bd10a3e7d69d13f8f530a4e"),
		flowClientTraceRoot("accel-withdraw-fix-1d5ce6cbe006-20261118-fanout-v1", "1d5ce6cbe00625e175aa953b41ae4efdab565c8bbdc0b65b3f4345653eac1172"),
		flowClientTraceRoot("accel-withdraw-fix-274a1c06a584-20261118-fanout-v1", "274a1c06a584325f3d85da4a602930b04d10455e39edbd665ccffad5e1ddf7d9"),
		flowClientTraceRoot("accel-withdraw-fix-47d139c1b3d1-20261118-fanout-v1", "47d139c1b3d180e45dec10d7511dbca4904142992c0bdb566cf4c92ae7997bec"),
		flowClientTraceRoot("accel-withdraw-fix-607606f87da1-20261118-fanout-v1", "607606f87da17ce8a8581cda77daff29c9c325a011b63fafc4a30fbf29efaff5"),
		flowClientTraceRoot("accel-withdraw-fix-b78785cb8f1b-20261118-fanout-v1", "b78785cb8f1bde3c896fb6a5c713955523ebe9f1420f911bd4b37fa793a69d23"),
		flowClientTraceRoot("accel-withdraw-fix-7a0116649b34-20261118-fanout-v1", "7a0116649b342e025886d35634fad63ab8f11e2d33d0cc9c92e759c5f600fcce"),
		flowClientTraceRoot("accel-withdraw-fix-173328c08451-20261118-fanout-v1", "173328c08451bcf4bacd8e374fd2d9a8efda54dafe9134ba3de9b16d74a9e251"),
		flowClientTraceRoot("accel-withdraw-fix-dfca246c1569-20261118-fanout-v1", "dfca246c15690ca8f7a99d9263027ee7ab4e784cad9829d8dfe493f7997f1d3a"),
		flowClientTraceRoot("accel-withdraw-fix-ae014b919dea-20261118-fanout-v1", "ae014b919dea62407563de2aba68a2eb8b267fb41d6d1ccc293833c93823e4bf"),
		flowSelfBondRoot("accel-withdraw-fix-1612d799cdf1-20261118-fanout-v5", "1612d799cdf188ad113cc346bb3174ee8f415271053c3b1086cc30359a082a7c"),
		flowClientTraceRoot("accel-withdraw-fix-0f096f7c01aa-20261118-fanout-v1", "0f096f7c01aae3024742d20c10a4c26ef0cd5b2755899cc9141597b7cac034aa"),
		flowClientTraceRoot("accel-withdraw-fix-e1d7823dd0ed-20261118-fanout-v1", "e1d7823dd0eda9921631776af4458732e0b07b3aa3717e15610016f9fcb2f79f"),
		flowClientTraceRoot("accel-withdraw-fix-9a1fedaa6b14-20261118-fanout-v1", "9a1fedaa6b144abab91593ac4ef48c4d435a6fa8b1370a1cb3a54f900406f3bc"),
		flowSelfBondRoot("accel-withdraw-fix-830507242368-20261118-fanout-v5", "8305072423682cbd2812b2013f4088c5f69f0f1b8d18729fb6228b5b94befea3"),
		flowClientTraceRoot("accel-withdraw-fix-79598717dab2-20261118-fanout-v1", "79598717dab2dc077ccea31a7bfcf578ca4df8b6d9bca33ec65b78c2b4a8188a"),
		flowClientTraceRoot("accel-withdraw-fix-27e4221ef4d7-20261118-fanout-v1", "27e4221ef4d71fb4d51fa184eb6baf685ae752a273d0a7db06ed88bebd13d30b"),
		flowSelfBondRoot("accel-withdraw-fix-3ab9af3f4ebc-20261118-fanout-v5", "3ab9af3f4ebc470c2ccb8bc44cd70d4dc7268bb4d049a35b05a741ab3ca801da"),
		flowClientTraceRoot("accel-withdraw-fix-51f876af2e17-20261118-fanout-v1", "51f876af2e17fdcc8a516c12deadfd0b2e4a3c34d17430d5a6746ad43c477475"),
		flowClientTraceRoot("accel-withdraw-fix-723a0b90d78c-20261118-fanout-v1", "723a0b90d78c1daefca133d782c5a2fbaf607816b2b55e4a188847a3eff9dbfe"),
		flowClientTraceRoot("accel-withdraw-fix-db8c046a7cfd-20261118-fanout-v1", "db8c046a7cfd1cddb83f24e7c079bcf59c6277652ccaacf7e7b725344a8a2965"),
		flowClientTraceRoot("accel-withdraw-fix-fb8abebe41cd-20261118-fanout-v1", "fb8abebe41cd055ba362fea92c683113137236ab7a8851ff99f194ec282ea6c4"),
		flowClientTraceRoot("accel-withdraw-fix-9f5f36d33ae6-20261118-fanout-v1", "9f5f36d33ae6fe5b0955445a9f4ceb731e5bf033cda7f5d7a724e4ddbacab6e2"),
		flowClientTraceRoot("accel-withdraw-fix-0c5d99323e2f-20261118-fanout-v1", "0c5d99323e2f9b31532e2b87cede615977d14251daf1fa3fe9c0cc28ad07ccbe"),
		flowClientTraceRoot("accel-withdraw-fix-a3f0ad366e6b-20261118-fanout-v1", "a3f0ad366e6b3c47f9d51d95d1c744418ecbbac84392e8e0ee6a1670765117c5"),
		flowClientTraceRoot("accel-withdraw-fix-75309bb748ad-20261118-fanout-v1", "75309bb748add122f8eec531c7705d93cf05d6433154e71080ea631cd956cb66"),
		flowAllFixesRoot("accel-withdraw-fix-81b068d2e7d5-20261118-fanout-v7", "81b068d2e7d5c5f0409a97eb2e324e6fc0ff1f8c89319907786aaf1905f9676d"),
		flowClientTraceRoot("accel-withdraw-fix-fcea1968b7af-20261118-fanout-v1", "fcea1968b7af091e011302f4924bd95858b19dd48001e6cf02fd700e691f5bcb"),
		flowClientTraceRoot("accel-withdraw-fix-8af885ef7203-20261118-fanout-v1", "8af885ef7203f0e9e32249b1580b75c12374417434c72ca643d534a8a64f7570"),
		flowClientTraceRoot("accel-withdraw-fix-384aec417e3f-20261118-fanout-v1", "384aec417e3f0ccef95dedd851776c7d6359e9d2513920db335acea154e90bf0"),
		flowClientTraceRoot("accel-withdraw-fix-4ca2882bbbbd-20261118-fanout-v1", "4ca2882bbbbd6d398efe095d7c7eb68dd99d65016af11b28003e168678e30984"),
		flowClientTraceRoot("accel-withdraw-fix-5f19c41a54c9-20261118-fanout-v1", "5f19c41a54c914ba7052c37689b2280c7f3351bab3e96cf8023f8e52fe1c6ec1"),
		flowClientTraceRoot("accel-withdraw-fix-b292507579a2-20261118-fanout-v1", "b292507579a2bd55aaf3ef483580bd89fb4a5996ad323ce485bb114af9916f95"),
		flowClientTraceRoot("accel-withdraw-fix-d0cea6448a0f-20261118-fanout-v1", "d0cea6448a0fc8fcf57f93d8296e1d3f7ec0a24a7572a1ce0c0434caed7e0946"),
		flowClientTraceRoot("accel-withdraw-fix-656214dd1c96-20261118-fanout-v1", "656214dd1c96ff4c1193d35707abbd667cc0053eda018134a3d531dff48291eb"),
		flowClientTraceRoot("accel-withdraw-fix-0c019c02d744-20261118-fanout-v1", "0c019c02d744f8202ad5771e5c22e143caaeb432a0cc81c62d81a18ae78a93de"),
		flowClientTraceRoot("accel-withdraw-fix-bb7050112168-20261118-fanout-v1", "bb7050112168b67a4ae2a62d5db9f0f27bfa83c0772458919caeedef5d0ea1d4"),
		flowClientTraceRoot("accel-withdraw-fix-f286ca180626-20261118-fanout-v1", "f286ca18062692eb61ec7f8d61a8e9c13ba245acacd1e6fe4efa15243e85d22c"),
		flowClientTraceRoot("accel-withdraw-fix-e5ede2045f7c-20261118-fanout-v1", "e5ede2045f7c06ec1da4432418d34d7f4f4a2a429d88531a60e0f7eb23a8cd64"),
		flowClientTraceRoot("accel-withdraw-fix-e1e0aecd720f-20261118-fanout-v1", "e1e0aecd720f6906e6988b490933cd76424582032eac8cc52ce9b0ce107baef3"),
		flowAllFixesRoot("accel-withdraw-fix-7fb6148ba662-20261118-fanout-v7", "7fb6148ba662ced14e3684c70cf15f21af97a37b3be54101364c32e5fad74c9f"),
		flowClientTraceRoot("accel-withdraw-fix-a69be940f481-20261118-fanout-v1", "a69be940f481d9a799aeedeb7c79faf8217ffd94e7f729b937ccce8aa085f0af"),
		flowSelfBondRoot("accel-withdraw-fix-6e8229a1bc25-20261118-fanout-v5", "6e8229a1bc254db678e1c7fe98994ee56e1e4015dde5bc78d546d841fa561521"),
		flowSelfBondRoot("accel-withdraw-fix-6b4f68929944-20261118-fanout-v5", "6b4f6892994436ad949b990c5f6a01169035628e9fd646540c190250ab2c0214"),
		flowSelfBondRoot("accel-withdraw-fix-82dfe9039f31-20261118-fanout-v1", "82dfe9039f3161eddf048bacc644251a1011f385a4999349ad787a46db03030f"),
		flowSelfBondRoot("accel-withdraw-fix-4244600c8e59-20261118-fanout-v1", "4244600c8e59578d0fbefa2567cd7db4495ed6550d6f2e6844be5732616a87c0"),
		// Drift improved from 24904.26 to 75.65 ATOM (overdraw 9 ATOM under both rules); remaining 75.6 ATOM traced as a likely contract-escrow ledger gap, fix in progress.
		flowAllFixesRoot("accel-withdraw-fix-413d90f184e9-20261118-fanout-v7", "413d90f184e958cc95e72638cf4d3920c765132a8bb473a2d7deed096685bc80"),
		// Zero-commission root: the served row has no commission either; its 6.81 ATOM drift came from an nft_sale credited at the gross price, which this build fixes.
		flowSelfBondRoot("accel-withdraw-fix-c926dfa85984-20261118-fanout-v5", "c926dfa85984a9d27733ce7d6a60004a24b3a6855a2a93d837b8e87a453d4a7d"),
		flowSelfBondRoot("accel-withdraw-fix-31c3561573d9-20261118-fanout-v1", "31c3561573d9829e2f337f69a707c40c5f936176352c5c6a7ebfe191ff96c185"),
		flowAllFixesRoot("accel-withdraw-fix-dc3a8dc3b69e-20261118-fanout-v7", "dc3a8dc3b69ed7ae16b967171f25c42d3add8f3f262d4520b7dfbaadcb8d321c"),
		flowSelfBondRoot("accel-withdraw-fix-b9ecc59cfc02-20261118-fanout-v1", "b9ecc59cfc02e29fcf76366fbafd10b9aab131541474dbb1cb137af46acce3c1"),
		flowSelfBondRoot("accel-withdraw-fix-4edb7be0f345-20261118-fanout-v1", "4edb7be0f345546c77f2037c549db6bc90490670785f0d531f6191d92d7d1b50"),
		flowClientTraceRoot("accel-withdraw-fix-4237d016eff9-20261118-fanout-v1", "4237d016eff9f444ac44a928b6fbb92af51023fd0796e3e06b59fda674380996"),
		flowClientTraceRoot("accel-withdraw-fix-a70de707d95f-20261118-fanout-v4", "a70de707d95fcb186b95515fa66770eff0b58c54a98ad281139d345ba953e2a0"),
		flowSlashRoot("accel-withdraw-fix-1af3d755b507-20261118-fanout-v1", "1af3d755b5076aaae9cc46334b7f319e4d67159c92b8b46472f3bf4f3c00643d"),
		flowSlashRoot("accel-withdraw-fix-67c999d116ef-20261118-fanout-v1", "67c999d116efad9e0f4bb723dbfccc9361424d5fb291f1385850b7a73885caeb"),
		flowSlashRoot("accel-withdraw-fix-a1cccb7ecdaa-20261118-fanout-v1", "a1cccb7ecdaaa393785ca8490635b8275f67b90166d3f2711c8dfba60d576d15"),
		// Frozen root served on its slash-build rerun; the old row overdrew 5.53 ATOM, and removing it moves about 3.9 ATOM from fees to restaked_own (10.97 pct class share).
		flowSlashRoot("accel-withdraw-fix-b81cd9164582-20261118-fanout-v1", "b81cd916458286253f8db74313138b16cbbda05880c8e9e28e338a4935013498"),
		flowSlashRoot("accel-withdraw-fix-b8a48e7e45e3-20261118-fanout-v1", "b8a48e7e45e3770ea9538325f8882b10746fee7a512e69b68fadc72c62f2dd5b"),
		flowSlashRoot("accel-withdraw-fix-9514e5e20dd8-20261118-fanout-v1", "9514e5e20dd865faef28bb1ea76698d30acfbe03b81e365a1b55a25114e707a4"),
		flowAllFixesRoot("accel-withdraw-fix-a106a4ef7b92-20261118-fanout-v7", "a106a4ef7b922fd9efd0d754bd7b354e997a647e814d2eabed9f920aa520af23"),
		flowSlashRoot("accel-withdraw-fix-d29d6d635b9c-20261118-fanout-v1", "d29d6d635b9c4007dc01785d3a40e37966692f11206b4988c45b8dd07ab50589"),
		flowSlashRoot("accel-withdraw-fix-91839f8b6913-20261118-fanout-v2", "91839f8b6913340a41e7915e84d50818dcadff35b8a7a04e3b367a4cb098fb3b"),
		flowAllFixesRoot("accel-withdraw-fix-ff3cb6404c89-20261118-fanout-v7", "ff3cb6404c897214795e05fbf454959486d59fce45dec6d3c8b4b22b06eb5dc7"),
		flowSlashRoot("accel-withdraw-fix-cd2bce46a2f4-20261118-fanout-v1", "cd2bce46a2f4665cb28de5bd6c35dc1b8ca75ec39b9732721756afa14f4b028f"),
		flowAllFixesRoot("accel-withdraw-fix-b386e14376df-20261118-fanout-v7", "b386e14376dfbcd911606ecce2d3e0622c75787e767ab35617d5248728063798"),
		flowSlashRoot("accel-withdraw-fix-cf7c27e96980-20261118-fanout-v1", "cf7c27e96980a328455579f0a80a1754e6361899d2df7141d928cc3dd68a7314"),
		flowSlashRoot("accel-withdraw-fix-3684fe44bf66-20261118-fanout-v1", "3684fe44bf665d6f717b4cf59249fa1bc45f83ddf66f7756e9b2bb3400912ff9"),
		flowSlashRoot("accel-withdraw-fix-c632deae5492-20261118-fanout-v3", "c632deae5492a2489182110830673329e9801311921a6be16a310f35312e0c19"),
		flowSlashRoot("accel-withdraw-fix-fcbf47285d98-20261118-fanout-v3", "fcbf47285d98335bf375bbd012f82e43d3126a0edc36e1f8bf992d9fc3eba2cc"),
		flowSlashRoot("accel-withdraw-fix-a3e755e2ec68-20261118-fanout-v3", "a3e755e2ec68cb558e6c4e69b4808153b484aaeb91721f8637ce1b6ddf9397d3"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "5e6e5429e48f651ba98b03e3791deda86e390d13112e503e902939d08d86aa10"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "e5ede2045f7c06ec1da4432418d34d7f4f4a2a429d88531a60e0f7eb23a8cd64"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "fcea1968b7af091e011302f4924bd95858b19dd48001e6cf02fd700e691f5bcb"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "274a1c06a584325f3d85da4a602930b04d10455e39edbd665ccffad5e1ddf7d9"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "86c5c4e665fef7b0671134a194322556b90ff8e12ee4a9f5e80839c7a300d1dc"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "e2f8b175c38b90fad6460e863546e042d20db6b5814f346936023922c2912c55"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "91839f8b6913340a41e7915e84d50818dcadff35b8a7a04e3b367a4cb098fb3b"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "2877ad667da5260e6188746799121924cb3fd1e13043450dad6192ec7387803a"),
		flowSlashRoot("accel-withdraw-fix-55b998caa294-20261118-fanout-v1", "55b998caa2945907f1b1177130043fd61b382dc3de466e51aacb22f058164509"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "8af885ef7203f0e9e32249b1580b75c12374417434c72ca643d534a8a64f7570"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "0c019c02d744f8202ad5771e5c22e143caaeb432a0cc81c62d81a18ae78a93de"),
		flowGiantRoot("accel-four-s02-20261118-fanout-v2", "bb7050112168b67a4ae2a62d5db9f0f27bfa83c0772458919caeedef5d0ea1d4"),
		flowGiantRoot("accel-four-s01-20261118-fanout-v2", "317097d52dd71d08f60d71a3622af4d4a442ebd012cd4e99d98367746de5b17d"),
		flowGiantRoot("accel-four-s01b-20261118-fanout-v2", "801dfe33c567eff0a1738d23a29982dabf76b1c85452210cb837315c7e818745"),
		flowGiantRoot("accel-four-s01-20261118-fanout-v2", "48b61e4b3eeb060cdbcea7dab31dff25bf04be77c344da0d32c0d6e68f8f70ba"),
		flowGiantRoot("accel-four-s01b-20261118-fanout-v2", "066df80725a01e1c27270b034e66486b6cd46d04d45e8e49b357c1a710db29b4"),
		flowGiantRoot("accel-four-s01b-20261118-fanout-v2", "31c1763c0d2bfaa85a33b9af53f0d7b9b8249cf2d225145ff2e3e23da2bc51be"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "4e09d1dfc7e6cdb78129a723dbc0d459052d8adbcc6e704a492520f5c05211f9"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "9cd3bff2c9449e24004bfe0fa0d08b8466fb60c4d7666e33a8826bf4e92b313e"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "11ad1e6695406f2ec282e61c3d2a68a278336c6d6c70a999f31aeb78b5315354"),
		flowGiantRoot("accel-four-s07-20261118-fanout-v2", "607606f87da17ce8a8581cda77daff29c9c325a011b63fafc4a30fbf29efaff5"),
		flowSlashRoot("accel-withdraw-fix-a21b65a9b88e-20261118-fanout-v1", "a21b65a9b88ec88fd0ad5015fe2907e2d89971bad4fe92d3808889b807198e1a"),
		// 320bcb826109 counts its tokenize-share record's reward payouts: drift
		// 0.19 ATOM on 84,949 ATOM, overdraw 0.19, shares unchanged.
		flowTokenizeRewardRoot("accel-withdraw-fix-320bcb826109-20261118-fanout-v6", "320bcb826109bdff2fcab865b3971af8023c5d8b2ba24f02961a4faca99425aa"),
		{
			Version: "ops818-unjail-profile930320029ed0-sources962174bfb014-s06-retry-20261115-v2",
			Build:   "sha256:fa71e36b2feec15498ec94b778e2571deebc1e11197a9da7b94968b658d3a874",
			Profile: "541329afb4e52d3d5f9a9503ed0851e4d66f780071630b8bfbd0dc2ab3fd46aa",
			FromDay: flowCorrectedFrom, AsOf: flowCorrectedAsOf,
			Root: "c07d4e7c6b6e288cc7c23111d04eca5c50872c5b58fdb9b5f2ec67ae2093cdad",
		},
		func() commissionServingSource {
			r := flowCorrected("accel-later-s02-20261115-deadline-retry-v2")
			r.Root = "2d473f4858d9987775254ce4bff8939f812bfefeb04eb1e6f0a5ebde72969fd8"
			return r
		}(),
		flowCorrectedRoot("accel-four-s05-20261115-deadline-retry-v4", "ab12977c1a2135f7aeea69534171ee3c2bec9a03f3650cbf5dff76caefb19345"),
		flowSlashRoot("accel-withdraw-fix-290eefd100eb-20261118-fanout-v1", "290eefd100eb62044e1da01618881c70ac9aa783672b9547415766c16bcf06ff"),
		flowCorrectedRoot("accel-four-s04-20261115-deadline-retry-v4", "0a11a4c0090436746a12555ec8f07a503c467e849bccf9ef550a8e2985f2b5ab"),
		flowCorrectedRoot("accel-later-s02-20261115-deadline-retry-v3", "50c56a74de37966c61278d7381538fe0d8fa3746d87eadae5d633a471f017de1"),
		flowCorrectedRoot("accel-later-s03-20261115-deadline-retry-v2", "001308de505138aa91ea460b148382f1dd7b6528a4a307e6333dff716f08a7ae"),
		flowCorrectedRoot("accel-four-s05-20261115-deadline-retry-v3", "5187c6ad32a6209ebd2f8b36e2f78b95950a96e046c474db04ca64b8c981f885"),
		flowCorrectedRoot("accel-four-s04-20261115-deadline-retry-v3", "6e79e2a8e3e5b77e7d96bc45c28592f36610c3992d54bb6e828ad44e375f1302"),
		flowLSMClientTraceRoot("accel-withdraw-fix-bc0089661f77-20261118-fanout-v4", "bc0089661f770388eeb5acd70a947bbc9c47ab0808b6fda90df95ff57800f15a"),
		flowLSMRoot("accel-four-s06-20261115-lsm-retry-v1", "85f6d8206eebe56d592d5c82aa2978a04e7edf1ad6fcfb8165e6b15020298fd6"),
		flowCorrected("accel-four-s01-20261115-v1"),
		flowCorrected("accel-four-s02-20261115-v1"),
		flowCorrected("accel-four-s03-20261115-v1"),
		flowCorrected("accel-four-s04-20261115-v1"),
		flowCorrected("accel-four-s05-20261115-v1"),
		flowCorrected("accel-four-s06-20261115-v1"),
		flowCorrected("accel-four-s07-20261115-v1"),
		flowCorrected("accel-later-s01-20261115-v1"),
		flowCorrected("accel-later-s02-20261115-v1"),
		flowCorrected("accel-later-s03-20261115-v1"),
		flowCorrected("accel-four-s02-20261115-split-b-v2"),
		flowCorrected("accel-four-s01-20261115-split-b-v2"),
		// Validators the stopped runs never reached, each list run on an idle host in its own version.
		flowCorrected("accel-four-s06-20261115-remainder-v1"),
		flowCorrected("accel-later-s02-20261115-remainder-v1"),
		flowCorrected("accel-later-s01-20261115-remainder-v1"),
		flowCorrected("full324-four-20261115-v2"),
		flowCorrected("full324-later-two-20261115-v2"),
	},
}

func (s *Server) commissionServingSources(chain string) []commissionServingSource {
	if s.flowSources != nil {
		return s.flowSources[chain]
	}
	return defaultCommissionServingSources[chain]
}

// flowVersionColumns are the only versioned columns the API reads.
const flowVersionColumns = "version, chain, validator, build, private_profile_sha256, computed_at, run_json::text AS run_json, rows_json::text AS rows_json"

type flowVersionRow struct {
	Version              string
	Chain                string
	Validator            string
	Build                string
	PrivateProfileSHA256 string
	ComputedAt           *time.Time
	RunJSON              string
	RowsJSON             string
}

// servedCommissionFlow is the result a validator's page and the Hub use.
type servedCommissionFlow struct {
	Run CommissionFlowRun
	// Rows are the versioned flow lines; nil for a legacy run, whose lines
	// stay in the legacy flow table.
	Rows      []CommissionFlow
	Versioned bool
}

func flowValidatorRoot(validator string) string {
	sum := sha256.Sum256([]byte(validator))
	return hex.EncodeToString(sum[:])
}

func flowDateText(t time.Time) string { return t.UTC().Format(apiDateLayout) }

// acceptFlowVersion decodes row and reports whether src may serve it.
func acceptFlowVersion(src commissionServingSource, row flowVersionRow, chain string) (servedCommissionFlow, bool) {
	if row.Version != src.Version || row.Chain != chain || row.Build != src.Build || row.PrivateProfileSHA256 != src.Profile {
		return servedCommissionFlow{}, false
	}
	if src.Root != "" && flowValidatorRoot(row.Validator) != src.Root {
		return servedCommissionFlow{}, false
	}
	// A chunked field that inlineFlowPayloads could not verify is refused.
	if isFlowPayloadMarker(row.RunJSON) || isFlowPayloadMarker(row.RowsJSON) {
		return servedCommissionFlow{}, false
	}
	var run CommissionFlowRun
	if json.Unmarshal([]byte(row.RunJSON), &run) != nil {
		return servedCommissionFlow{}, false
	}
	if run.Chain != chain || run.Validator != row.Validator || flowDateText(run.FromDay) != src.FromDay || flowDateText(run.AsOf) != src.AsOf {
		return servedCommissionFlow{}, false
	}
	switch run.Status {
	case flowStatusOK, flowStatusReconciled, flowStatusDrift:
	default:
		return servedCommissionFlow{}, false
	}
	rows := []CommissionFlow{}
	if json.Unmarshal([]byte(row.RowsJSON), &rows) != nil {
		return servedCommissionFlow{}, false
	}
	for _, f := range rows {
		if f.Chain != chain || f.Validator != row.Validator {
			return servedCommissionFlow{}, false
		}
		if _, err := decimal.NewFromString(f.AmountBase); err != nil {
			return servedCommissionFlow{}, false
		}
	}
	return servedCommissionFlow{Run: run, Rows: rows, Versioned: true}, true
}

// isFlowPayloadMarker matches the collector's chunked-payload placeholder,
// a JSON string in place of the field's object or array.
func isFlowPayloadMarker(raw string) bool {
	var value string
	return json.Unmarshal([]byte(raw), &value) == nil && strings.HasPrefix(value, "cosmos-commission-payload/")
}

// Run and rows fields over the collector's 1 MiB inline limit are stored as
// gzip chunks. The API reads them only through these views, which hold the
// run and rows payloads and never the private evidence payloads.
const (
	flowServedPayloadsView = "tax_commission_flow_served_payloads"
	flowServedChunksView   = "tax_commission_flow_served_payload_chunks"
	flowPayloadCodec       = "gzip-json-bytes-v1"
	flowPayloadChunkBytes  = 1 << 20
	flowPayloadMaxRaw      = 64 << 20
)

type flowPayloadIndex struct {
	Build         string
	Codec         string
	RawSHA256     string
	RawBytes      int64
	EncodedSHA256 string
	EncodedBytes  int64
	ChunkCount    int64
	ChunkBytes    int64
}

type flowPayloadChunk struct {
	Ordinal int64
	SHA256  string
	Data    []byte
}

// inlineFlowPayloads replaces a chunked run or rows marker with its verified
// JSON. A payload that cannot be read or verified keeps its marker, so
// acceptFlowVersion refuses the row exactly as before.
func (s *Server) inlineFlowPayloads(row *flowVersionRow) {
	for field, value := range map[string]*string{"run": &row.RunJSON, "rows": &row.RowsJSON} {
		if !isFlowPayloadMarker(*value) {
			continue
		}
		raw, err := s.readFlowPayload(*row, field, *value)
		if err != nil {
			if s.flowPayloadFailed.CompareAndSwap(false, true) {
				log.Printf("commission flows: chunked %s payload not served: %v", field, err)
			}
			continue
		}
		*value = raw
	}
}

// readFlowPayload rebuilds one chunked field, checking every ordered chunk,
// the encoded and raw lengths and hashes, and the marker against its index.
func (s *Server) readFlowPayload(row flowVersionRow, field, marker string) (string, error) {
	where := "version = ? AND chain = ? AND validator = ? AND field = ?"
	var m flowPayloadIndex
	if err := s.db.Table(flowServedPayloadsView).Select("build, codec, raw_sha256, raw_bytes, encoded_sha256, encoded_bytes, chunk_count, chunk_bytes").
		Where(where, row.Version, row.Chain, row.Validator, field).Take(&m).Error; err != nil {
		return "", err
	}
	want, _ := json.Marshal("cosmos-commission-payload/v1:" + field + ":" + m.RawSHA256)
	if m.Build != row.Build || m.Codec != flowPayloadCodec || string(want) != marker || m.ChunkBytes != flowPayloadChunkBytes ||
		m.RawBytes <= 0 || m.RawBytes > flowPayloadMaxRaw || m.EncodedBytes <= 0 || m.EncodedBytes > flowPayloadMaxRaw ||
		m.ChunkCount != (m.EncodedBytes-1)/m.ChunkBytes+1 {
		return "", fmt.Errorf("payload index does not match its row")
	}
	var chunks []flowPayloadChunk
	if err := s.db.Table(flowServedChunksView).Select("ordinal, sha256, data").
		Where(where, row.Version, row.Chain, row.Validator, field).Order("ordinal ASC").Scan(&chunks).Error; err != nil {
		return "", err
	}
	if int64(len(chunks)) != m.ChunkCount {
		return "", fmt.Errorf("payload has %d of %d chunks", len(chunks), m.ChunkCount)
	}
	encoded := make([]byte, 0, m.EncodedBytes)
	for i, c := range chunks {
		sum := sha256.Sum256(c.Data)
		last := int64(i) == m.ChunkCount-1
		if c.Ordinal != int64(i) || len(c.Data) == 0 || int64(len(c.Data)) > m.ChunkBytes || (!last && int64(len(c.Data)) != m.ChunkBytes) || hex.EncodeToString(sum[:]) != c.SHA256 {
			return "", fmt.Errorf("payload chunk %d out of order or corrupt", i)
		}
		encoded = append(encoded, c.Data...)
	}
	sum := sha256.Sum256(encoded)
	if int64(len(encoded)) != m.EncodedBytes || hex.EncodeToString(sum[:]) != m.EncodedSHA256 {
		return "", fmt.Errorf("payload encoded length or hash mismatch")
	}
	zr, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	zr.Multistream(false)
	raw, err := io.ReadAll(io.LimitReader(zr, m.RawBytes+1))
	if err != nil {
		return "", err
	}
	sum = sha256.Sum256(raw)
	if int64(len(raw)) != m.RawBytes || hex.EncodeToString(sum[:]) != m.RawSHA256 || !json.Valid(raw) {
		return "", fmt.Errorf("payload raw length or hash mismatch")
	}
	return string(raw), nil
}

// acceptedFlowVersions returns, per validator, the accepted versioned result
// from the highest-priority collection. validator limits the read to one.
func (s *Server) acceptedFlowVersions(chain, validator string) (map[string]servedCommissionFlow, error) {
	sources := s.commissionServingSources(chain)
	out := map[string]servedCommissionFlow{}
	if len(sources) == 0 {
		return out, nil
	}
	versions := make([]string, len(sources))
	for i, src := range sources {
		versions[i] = src.Version
	}
	// Versioned results are only ever inserted, so the row count and newest
	// computed_at identify the whole set; the Hub reuses its decoded set
	// until either changes.
	var stamp flowVersionStamp
	if validator == "" {
		if err := s.db.Table(flowVersionsTable).Select("count(validator) AS n, max(computed_at) AS latest").
			Where("version IN ? AND chain = ?", versions, chain).Scan(&stamp).Error; err != nil {
			return s.flowVersionsUnreadable(out, err)
		}
		s.flowVerMu.Lock()
		c, ok := s.flowVer[chain]
		s.flowVerMu.Unlock()
		if ok && c.stamp.equal(stamp) && c.sources == strings.Join(versions, ",") {
			return c.served, nil
		}
	}
	q := s.db.Table(flowVersionsTable).Select(flowVersionColumns).Where("version IN ? AND chain = ?", versions, chain)
	if validator != "" {
		q = q.Where("validator = ?", validator)
	}
	var rows []flowVersionRow
	if err := q.Scan(&rows).Error; err != nil {
		return s.flowVersionsUnreadable(out, err)
	}
	for i := range rows {
		s.inlineFlowPayloads(&rows[i])
	}
	// The cached set is stamped with what was actually read, so a row
	// inserted between the two reads only causes one more read next time.
	stamp = flowVersionStamp{N: int64(len(rows))}
	for _, r := range rows {
		if r.ComputedAt != nil && (stamp.Latest == nil || r.ComputedAt.After(*stamp.Latest)) {
			t := *r.ComputedAt
			stamp.Latest = &t
		}
	}
	byVersion := map[string][]flowVersionRow{}
	for _, r := range rows {
		byVersion[r.Version] = append(byVersion[r.Version], r)
	}
	for _, src := range sources {
		list := byVersion[src.Version]
		sort.Slice(list, func(i, j int) bool { return list[i].Validator < list[j].Validator })
		for _, r := range list {
			if _, done := out[r.Validator]; done {
				continue
			}
			if served, ok := acceptFlowVersion(src, r, chain); ok {
				out[r.Validator] = served
			}
		}
	}
	if validator == "" {
		s.flowVerMu.Lock()
		if s.flowVer == nil {
			s.flowVer = map[string]flowVersionCache{}
		}
		s.flowVer[chain] = flowVersionCache{stamp: stamp, sources: strings.Join(versions, ","), served: out}
		s.flowVerMu.Unlock()
	}
	return out, nil
}

// flowVersionsUnreadable lets the page keep serving the legacy runs, exactly
// as before versioned results existed, while the database has no versioned
// table (the collectors create it) or the role has not been granted its
// columns. Any other read error fails the request.
func (s *Server) flowVersionsUnreadable(empty map[string]servedCommissionFlow, err error) (map[string]servedCommissionFlow, error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == pgInsufficientPrivilege || pgErr.Code == pgUndefinedTable) {
		if s.flowVerDenied.CompareAndSwap(false, true) {
			log.Printf("commission flows: versioned results not readable, serving legacy runs only: %v", err)
		}
		return empty, nil
	}
	return nil, fmt.Errorf("read versioned commission results: %w", err)
}

const (
	pgInsufficientPrivilege = "42501"
	pgUndefinedTable        = "42P01"
)

type flowVersionStamp struct {
	N      int64
	Latest *time.Time
}

func (a flowVersionStamp) equal(b flowVersionStamp) bool {
	if a.N != b.N || (a.Latest == nil) != (b.Latest == nil) {
		return false
	}
	return a.Latest == nil || a.Latest.Equal(*b.Latest)
}

// flowVersionCache is a decoded accepted set; callers never modify served.
type flowVersionCache struct {
	stamp   flowVersionStamp
	sources string
	served  map[string]servedCommissionFlow
}

const flowVersionsTable = "tax_commission_flow_versions"

// servedCommissionFlows merges accepted versioned results over the legacy
// runs: a validator with an accepted versioned result is served from it,
// any other validator keeps its legacy run.
func (s *Server) servedCommissionFlows(chain, validator string) (map[string]servedCommissionFlow, error) {
	accepted, err := s.acceptedFlowVersions(chain, validator)
	if err != nil {
		return nil, err
	}
	out := make(map[string]servedCommissionFlow, len(accepted))
	for v, sv := range accepted {
		out[v] = sv
	}
	var runs []CommissionFlowRun
	q := s.db.Where("chain = ?", chain)
	if validator != "" {
		q = q.Where("validator = ?", validator)
	}
	if err := q.Find(&runs).Error; err != nil {
		return nil, err
	}
	for _, run := range runs {
		if _, ok := out[run.Validator]; !ok {
			out[run.Validator] = servedCommissionFlow{Run: run}
		}
	}
	return out, nil
}

// servedFlowLines returns the served flow lines for rule: versioned lines
// from the result itself and legacy lines from the legacy flow table.
func (s *Server) servedFlowLines(chain, validator string, served servedCommissionFlow, rule string) ([]CommissionFlow, error) {
	if served.Versioned {
		out := []CommissionFlow{}
		for _, f := range served.Rows {
			if f.Rule == rule {
				out = append(out, f)
			}
		}
		return out, nil
	}
	var flows []CommissionFlow
	err := s.db.Where("chain = ? AND validator = ? AND rule = ?", chain, validator, rule).Find(&flows).Error
	return flows, err
}

// hubFlowLines sums the served lines for rule over the validators in include,
// grouped by month, class, detail and route.
func (s *Server) hubFlowLines(chain, rule string, served map[string]servedCommissionFlow, include map[string]bool) ([]CommissionFlow, error) {
	type key struct {
		month, class, detail string
		via                  bool
	}
	sums := map[key]decimal.Decimal{}
	var legacy []string
	for v := range include {
		sv := served[v]
		if !sv.Versioned {
			legacy = append(legacy, v)
			continue
		}
		for _, f := range sv.Rows {
			if f.Rule != rule {
				continue
			}
			amt, _ := decimal.NewFromString(f.AmountBase)
			k := key{f.Month, f.Class, f.Detail, f.ViaUnbonding}
			sums[k] = sums[k].Add(amt)
		}
	}
	if len(legacy) > 0 {
		sort.Strings(legacy)
		var flows []CommissionFlow
		if err := s.db.Table(CommissionFlow{}.TableName()).
			Select("month, class, detail, via_unbonding, SUM(amount_base)::text AS amount_base").
			Where("chain = ? AND rule = ? AND validator IN ?", chain, rule, legacy).
			Group("month, class, detail, via_unbonding").Scan(&flows).Error; err != nil {
			return nil, err
		}
		for _, f := range flows {
			amt, err := decimal.NewFromString(f.AmountBase)
			if err != nil {
				return nil, fmt.Errorf("legacy flow amount: %w", err)
			}
			k := key{f.Month, f.Class, f.Detail, f.ViaUnbonding}
			sums[k] = sums[k].Add(amt)
		}
	}
	keys := make([]key, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.month != b.month {
			return a.month < b.month
		}
		if a.class != b.class {
			return a.class < b.class
		}
		if a.detail != b.detail {
			return a.detail < b.detail
		}
		return !a.via && b.via
	})
	out := make([]CommissionFlow, 0, len(keys))
	for _, k := range keys {
		out = append(out, CommissionFlow{Chain: chain, Rule: rule, Month: k.month, Class: k.class, Detail: k.detail, ViaUnbonding: k.via, AmountBase: sums[k].String()})
	}
	return out, nil
}
