// taxwatch is the Cosmos Hub release watcher for the tax layer. It compares
// three sources with the reviewed release registry in package tax:
//
//   - GitHub releases of cosmos/gaia (latest stable and latest prerelease)
//   - the node's reported binary and Cosmos SDK versions
//   - the node's scheduled upgrade plan (x/upgrade current_plan), or the
//     optional upgrades API named by UPGRADES_API_URL
//
// Every run is recorded as a taxapi.ChainReleaseCheck row so the API can
// serve it at GET /chain/releases. The process exits non-zero when a release
// exists that the tax layer has not reviewed, so a CronJob failure is the
// alert.
//
// Env: DB_HOST DB_PORT DB_NAME DB_USER DB_PASS, NODE_REST_API (chain REST
// host), GITHUB_REPO (default cosmos/gaia), UPGRADES_API_URL (optional JSON
// endpoint; any object carrying name/height or version/upgrade_height fields),
// HTTP_TIMEOUT (default 30s).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	indexerDB "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/DefiantLabs/cosmos-indexer/taxapi"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var client = &http.Client{Timeout: 30 * time.Second}

func getJSON(url string, out any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "bryanlabs-taxwatch/1.0")
	if strings.Contains(url, "api.github.com") {
		if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type ghRelease struct {
	TagName     string `json:"tag_name"`
	Prerelease  bool   `json:"prerelease"`
	Draft       bool   `json:"draft"`
	PublishedAt string `json:"published_at"`
}

// upstream returns the newest stable tag, its date, and the newest prerelease
// tag that is newer than the stable one.
func upstream(repo string) (stable, date, pre string, err error) {
	var rels []ghRelease
	if err = getJSON("https://api.github.com/repos/"+repo+"/releases?per_page=30", &rels); err != nil {
		return
	}
	for _, r := range rels {
		if r.Draft {
			continue
		}
		if r.Prerelease {
			if pre == "" {
				pre = r.TagName
			}
			continue
		}
		stable, date = r.TagName, strings.SplitN(r.PublishedAt, "T", 2)[0]
		break
	}
	if pre != "" && stable != "" && !newer(pre, stable) {
		pre = ""
	}
	return
}

// newer reports whether tag a sorts after tag b by numeric major.minor.patch.
// Prerelease suffixes (-rc0) sort before their final release.
func newer(a, b string) bool {
	pa, sa := parts(a)
	pb, sb := parts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	if sa == sb {
		return false
	}
	// Same numbers: a final release is newer than its prerelease.
	return sa == ""
}

func parts(tag string) ([3]int, string) {
	var n [3]int
	tag = strings.TrimPrefix(tag, "v")
	suffix := ""
	if i := strings.IndexAny(tag, "-+"); i >= 0 {
		suffix = tag[i:]
		tag = tag[:i]
	}
	for i, p := range strings.SplitN(tag, ".", 3) {
		n[i], _ = strconv.Atoi(p)
	}
	return n, suffix
}

type nodeInfo struct {
	ApplicationVersion struct {
		Version   string `json:"version"`
		CosmosSDK string `json:"cosmos_sdk_version"`
	} `json:"application_version"`
}

type currentPlan struct {
	Plan *struct {
		Name   string `json:"name"`
		Height string `json:"height"`
		Info   string `json:"info"`
	} `json:"plan"`
}

type latestBlock struct {
	Block struct {
		Header struct {
			Height string `json:"height"`
		} `json:"header"`
	} `json:"block"`
}

func nodeState(rest string) (version, sdk string, height int64, planName string, planHeight int64, errs []string) {
	var ni nodeInfo
	if err := getJSON(rest+"/cosmos/base/tendermint/v1beta1/node_info", &ni); err != nil {
		errs = append(errs, "node_info: "+err.Error())
	} else {
		version, sdk = ni.ApplicationVersion.Version, ni.ApplicationVersion.CosmosSDK
	}
	var lb latestBlock
	if err := getJSON(rest+"/cosmos/base/tendermint/v1beta1/blocks/latest", &lb); err != nil {
		errs = append(errs, "latest block: "+err.Error())
	} else {
		height, _ = strconv.ParseInt(lb.Block.Header.Height, 10, 64)
	}
	var cp currentPlan
	if err := getJSON(rest+"/cosmos/upgrade/v1beta1/current_plan", &cp); err != nil {
		errs = append(errs, "current_plan: "+err.Error())
	} else if cp.Plan != nil {
		planName = cp.Plan.Name
		planHeight, _ = strconv.ParseInt(cp.Plan.Height, 10, 64)
	}
	return
}

// upgradesAPI reads an optional external upgrades feed. It accepts either a
// single object or a list, and the first entry with a height in the future
// wins. Field names follow the common upgrade-tracker shape.
func upgradesAPI(url string, nodeHeight int64) (name string, height int64, err error) {
	var raw json.RawMessage
	if err = getJSON(url, &raw); err != nil {
		return
	}
	var list []map[string]any
	if json.Unmarshal(raw, &list) != nil {
		var one map[string]any
		if err = json.Unmarshal(raw, &one); err != nil {
			return
		}
		list = []map[string]any{one}
	}
	for _, m := range list {
		if cid, ok := m["chain_id"].(string); ok && cid != "" && cid != "cosmoshub-4" {
			continue
		}
		h := numField(m, "upgrade_height", "height", "block_height")
		if h <= nodeHeight {
			continue
		}
		n := strField(m, "version", "name", "upgrade_name", "plan_name")
		return n, h, nil
	}
	return "", 0, nil
}

func numField(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func normalizeTag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

func main() {
	if t := os.Getenv("HTTP_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			client.Timeout = d
		}
	}
	db, err := indexerDB.PostgresDbConnect(env("DB_HOST", "localhost"), env("DB_PORT", "5432"), env("DB_NAME", "indexer"), env("DB_USER", "postgres"), env("DB_PASS", "postgres"), env("DB_LOG_LEVEL", "warn"))
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	if err := db.AutoMigrate(&taxapi.ChainReleaseCheck{}); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	check := taxapi.ChainReleaseCheck{CheckedAt: time.Now().UTC()}
	var notes, unreviewed []string
	reviewed := tax.LatestKnownGaiaRelease().Version

	stable, date, pre, err := upstream(env("GITHUB_REPO", "cosmos/gaia"))
	if err != nil {
		notes = append(notes, "github: "+err.Error())
	}
	check.UpstreamLatest, check.UpstreamLatestDate, check.UpstreamPrerelease = stable, date, pre

	rest := strings.TrimRight(env("NODE_REST_API", "http://localhost:1317"), "/")
	version, sdk, height, planName, planHeight, errs := nodeState(rest)
	notes = append(notes, errs...)
	check.NodeVersion, check.NodeSDKVersion, check.NodeHeight = normalizeTag(version), sdk, height
	check.PendingUpgradeName, check.PendingUpgradeHeight = planName, planHeight

	if u := os.Getenv("UPGRADES_API_URL"); u != "" {
		if n, h, err := upgradesAPI(u, height); err != nil {
			notes = append(notes, "upgrades api: "+err.Error())
		} else if h > 0 && check.PendingUpgradeHeight == 0 {
			check.PendingUpgradeName, check.PendingUpgradeHeight = n, h
			notes = append(notes, "pending upgrade taken from the upgrades API")
		}
	}

	consider := func(label, tag string) {
		tag = normalizeTag(tag)
		if tag == "" {
			return
		}
		if r, ok := tax.KnownGaiaRelease(tag); ok && r.Reviewed {
			return
		}
		if _, ok := tax.KnownGaiaRelease(tag); !ok && !newer(tag, reviewed) {
			// Older than the newest reviewed release and not in the registry:
			// a patch release the Hub never ran. Note it without alarm.
			notes = append(notes, fmt.Sprintf("%s %s predates the reviewed %s and is not in the registry", label, tag, reviewed))
			return
		}
		unreviewed = append(unreviewed, label+":"+tag)
	}
	consider("upstream", stable)
	consider("prerelease", pre)
	consider("node", version)
	if check.PendingUpgradeName != "" {
		consider("pending", check.PendingUpgradeName)
	}
	if r := tax.LatestKnownGaiaRelease(); sdk != "" && !strings.HasPrefix(strings.TrimPrefix(sdk, "v"), majorMinor(r.SDK)) {
		notes = append(notes, fmt.Sprintf("node runs cosmos-sdk %s; the reviewed %s was built on %s: re-check event attribute names", sdk, r.Version, r.SDK))
		check.Attention = true
	}

	check.UnreviewedReleases = strings.Join(unreviewed, ",")
	if len(unreviewed) > 0 {
		check.Attention = true
	}
	check.Notes = strings.Join(notes, "; ")
	if err := db.Create(&check).Error; err != nil {
		log.Fatalf("record check: %v", err)
	}
	out, _ := json.Marshal(check)
	fmt.Println(string(out))
	if check.Attention {
		fmt.Fprintf(os.Stderr, "attention: unreviewed releases %q; notes: %s\n", check.UnreviewedReleases, check.Notes)
		os.Exit(2)
	}
}

func majorMinor(v string) string {
	p := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(p) < 2 {
		return v
	}
	return p[0] + "." + p[1]
}
