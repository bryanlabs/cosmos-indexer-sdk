package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/stretchr/testify/require"
)

const (
	rowKeyTx = "ABC"
	abc01    = "ABC#0#1"
	abcUatom = "ABC:uatom"
)

func keyedRow(category, denom string, message uint, messageIndex, sub int) Row {
	r := protoRow(category, "in", apiSymbolATOM, 1, 1, rowKeyTx)
	r.Denom, r.MessageID, r.MessageIndex, r.SubIndex, r.hasMessageIndex = denom, message, messageIndex, sub, true
	return r
}

// rowKeyRows is one transaction: a reward and a transfer in message 0, a
// transfer of another asset in message 1, the withdraw-address move of the
// reward and the fee.
func rowKeyRows() []Row {
	move := keyedRow(apiTransfer, apiUatom, 10, 0, 2)
	move.RewardTrigger = tax.TriggerWithdrawAddress
	fee := keyedRow(apiFee, apiUatom, 0, 0, 0)
	fee.hasMessageIndex = false
	return []Row{
		keyedRow(apiReward, apiUatom, 10, 0, 0),
		keyedRow(apiTransfer, apiUatom, 10, 0, 1),
		keyedRow(apiTransfer, "uosmo", 11, 1, 0),
		move,
		fee,
	}
}

func userTagsOf(rows []Row, tags UserTags) []string {
	copied := append([]Row(nil), rows...)
	out := make([]string, len(copied))
	for i, r := range ApplyTags(copied, tags) {
		out[i] = r.UserTag
	}
	return out
}

func TestRowTagKeys(t *testing.T) {
	tags, err := ParseUserTags(`{"abc#1#-3":"gift","abc#01#2":"spam","abc":"payment","abc:uatom":"income"}`)
	require.NoError(t, err)
	require.Equal(t, UserTags{"ABC#1#-3": TagGift, "ABC#1#2": TagSpam, rowKeyTx: TagPayment, abcUatom: TagIncome}, tags)
	for _, bad := range []string{"abc#x#1", "abc#-1#0", "abc#1", "abc#1#2#3", "abc:uatom#1#2", "#1#2", "abc#1#"} {
		_, err := ParseUserTags(`{"` + bad + `":"gift"}`)
		require.Error(t, err, bad)
	}

	// A row key wins over the asset key, which wins over the transaction key.
	// The move and the fee take no tag under any form, their own row key
	// included.
	rows := rowKeyRows()
	tags = UserTags{rowKeyTx: TagPayment, abcUatom: TagIncome, abc01: TagGift, "ABC#0#2": TagGift, "ABC#0#0": TagSpam}
	require.Equal(t, []string{TagSpam, TagGift, TagPayment, "", ""}, userTagsOf(rows, tags))

	// Without its message index a row has no row key and falls back.
	rows[1].hasMessageIndex = false
	require.Equal(t, []string{TagSpam, TagIncome, TagPayment, "", ""}, userTagsOf(rows, tags))
}

func TestMigrateUserTagsMatchesApplyTags(t *testing.T) {
	rows := rowKeyRows()
	unloaded := protoRow(apiTransfer, "in", apiSymbolATOM, 1, 1, "DEF")
	unloaded.MessageID = 20
	rows = append(rows, unloaded)
	old := UserTags{rowKeyTx: TagPayment, abcUatom: TagIncome, abc01: TagGift, "DEF": TagSpam, "GHI": TagGift}

	migrated, counts, err := MigrateUserTags(rows, old)
	require.NoError(t, err)
	// Every row the old tags reached gets its row key; the move and the fee
	// get none. The transaction and asset keys of ABC are replaced. DEF's row
	// has no row key, so its key stays, and GHI has no row here.
	require.Equal(t, UserTags{"ABC#0#0": TagIncome, abc01: TagGift, "ABC#1#0": TagPayment, "DEF": TagSpam, "GHI": TagGift}, migrated)
	require.Equal(t, TagMigration{RowKeys: 2, Dropped: 2, Kept: 3}, counts)
	require.Equal(t, userTagsOf(rows, old), userTagsOf(rows, migrated))
	require.Equal(t, []string{TagIncome, TagGift, TagPayment, "", "", TagSpam}, userTagsOf(rows, migrated))

	// A row a later replay adds to the transaction inherits the old asset
	// tag, and nothing from the migrated tags.
	added := append(slices.Clone(rows), keyedRow(apiTransfer, apiUatom, 10, 0, 3))
	require.Equal(t, TagIncome, userTagsOf(added, old)[len(added)-1])
	require.Empty(t, userTagsOf(added, migrated)[len(added)-1])

	// Migrating again changes nothing.
	again, counts, err := MigrateUserTags(rows, migrated)
	require.NoError(t, err)
	require.Equal(t, migrated, again)
	require.Equal(t, TagMigration{Kept: 5}, counts)

	// The same row in two wallets of one report keeps one key.
	twice := append(slices.Clone(rows), rows[0])
	got, counts, err := MigrateUserTags(twice, old)
	require.NoError(t, err)
	require.Equal(t, migrated, got)
	require.Equal(t, 2, counts.RowKeys)

	// Two rows that would give one row key different tags are refused.
	clash := keyedRow(apiTransfer, "uosmo", 10, 0, 0)
	_, _, err = MigrateUserTags(append(slices.Clone(rows), clash), old)
	require.ErrorContains(t, err, "ABC#0#0")
}

// The report cache key hashes the canonical tags, so a report under its
// migrated tags is a new key with the same rows and tags.
func TestReportJobKeyWithMigratedTags(t *testing.T) {
	rows := rowKeyRows()
	old := UserTags{rowKeyTx: TagPayment}
	migrated, _, err := MigrateUserTags(rows, old)
	require.NoError(t, err)
	key := func(tags UserTags) string {
		k, err := reportJobKey(apiMainnet, []string{"cosmos1a"}, "", "", apiCryptotaxcalculator, nil, tags)
		require.NoError(t, err)
		return k
	}
	require.NotEqual(t, key(old), key(migrated))
	reparsed, err := ParseUserTags(`{"abc#1#0":"payment","abc#0#1":"payment","abc#0#0":"payment"}`)
	require.NoError(t, err)
	require.Equal(t, key(migrated), key(reparsed))
	require.NotContains(t, key(nil), "|tags:")
}

func callTags(t *testing.T, s *Server, path, wallet string, tags UserTags) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(tags)
	require.NoError(t, err)
	q := url.Values{apiAddresses: {wallet}, apiChain: {apiMainnet}, apiStart: {apiTestPeriodStart}, apiEnd: {apiTestPeriodEnd}, apiFormat: {apiCryptotaxcalculator}, apiTags: {string(b)}}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(apiMethodGET, path+"?"+q.Encode(), nil))
	return rr
}

func waitTaggedPreview(t *testing.T, s *Server, wallet string, tags UserTags) string {
	t.Helper()
	for range 100 {
		rr := callTags(t, s, apiReportJob, wallet, tags)
		require.Equal(t, 200, rr.Code, rr.Body.String())
		var data map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &data))
		require.NotEqual(t, ReportJobFailed, data[apiStatus], data)
		if data[apiStatus] == ReportJobReady {
			return data[apiCSV].(string)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("preview did not complete")
	return ""
}

const tagMigratePath = "/tags/migrate"

func postMigrate(t *testing.T, s *Server, wallet, tags, scopes string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"chain":"` + apiMainnet + `","addresses":["` + wallet + `"],"start":"` + apiTestPeriodStart + `","end":"` + apiTestPeriodEnd + `","tags":` + tags + `}`
	req := httptest.NewRequest(http.MethodPost, tagMigratePath, strings.NewReader(body))
	if scopes != "" {
		req.Header.Set(TagScopesHeader, scopes)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

// POST /tags/migrate reads the stored message index, returns the row keys,
// and the preview under them is the preview under the old tags.
func TestTagMigrateEndpoint(t *testing.T) {
	s, db, wallet, _ := assetReviewFixture(t)
	require.NoError(t, db.Exec("UPDATE messages SET message_index = 3 WHERE id = 1").Error)

	tags := `{"scamtx":"spam","OTHERTX":"gift"}`
	// Migration is a POST and needs tax:write.
	rr := callTags(t, s, tagMigratePath, wallet, UserTags{"scamtx": TagSpam})
	require.Equal(t, 405, rr.Code, rr.Body.String())
	for _, scopes := range []string{"", "tax:read", "tax:writer", "tax:read tax:admin"} {
		rr = postMigrate(t, s, wallet, tags, scopes)
		require.Equal(t, 403, rr.Code, scopes)
	}
	rr = postMigrate(t, s, wallet, tags, "tax:read, tax:write")
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var got struct {
		Tags      UserTags     `json:"tags"`
		Migration TagMigration `json:"migration"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, UserTags{"SCAMTX#3#0": TagSpam, "OTHERTX": TagGift}, got.Tags)
	require.Equal(t, TagMigration{RowKeys: 1, Dropped: 1, Kept: 1}, got.Migration)

	before := waitTaggedPreview(t, s, wallet, UserTags{"SCAMTX": TagSpam})
	after := waitTaggedPreview(t, s, wallet, got.Tags)
	require.Equal(t, before, after)
	records := csvRecords(t, after)
	col := slices.Index(records[0], "User Tag")
	require.Positive(t, col)
	require.Equal(t, TagSpam, records[1][col])
	var jobs int64
	require.NoError(t, db.Model(&ReportJob{}).Count(&jobs).Error)
	require.EqualValues(t, 2, jobs)

	// A row key with the wrong message index tags nothing.
	stale := csvRecords(t, waitTaggedPreview(t, s, wallet, UserTags{"SCAMTX#0#0": TagSpam}))
	require.Empty(t, stale[1][col])

	for _, bad := range []string{`{"SCAMTX#x#0":"spam"}`, `{"SCAMTX":"nonsense"}`, `[]`} {
		rr = postMigrate(t, s, wallet, bad, TagWriteScope)
		require.Equal(t, 400, rr.Code, bad)
	}
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, tagMigratePath, strings.NewReader(`{"addresses":["`+wallet+`"],"tags":{},"extra":1}`))
	req.Header.Set(TagScopesHeader, TagWriteScope)
	s.Handler().ServeHTTP(rr, req)
	require.Equal(t, 400, rr.Code, "unknown fields are refused")
}
