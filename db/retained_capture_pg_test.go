package db_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/core"
	indexdb "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/cosmos-indexer/filter"
	"github.com/DefiantLabs/cosmos-indexer/parsers"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/DefiantLabs/cosmos-indexer/testsupport/nativepg"
	"github.com/DefiantLabs/probe/client"
	tmjson "github.com/cometbft/cometbft/libs/json"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	ica "github.com/cosmos/ibc-go/v7/modules/apps/27-interchain-accounts"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type captureManifest struct {
	Shards []struct {
		Shard    int
		Selected []struct {
			Height       int64
			Transactions int
		}
	}
}
type captureStats struct {
	Shard        int            `json:"shard"`
	Blocks       int            `json:"blocks"`
	Txs          int            `json:"txs"`
	Messages     int            `json:"messages"`
	Events       int            `json:"events"`
	Attributes   int            `json:"attributes"`
	Opaque       int            `json:"opaque"`
	ParserErrors int            `json:"parser_errors"`
	BeginEvents  int            `json:"begin_events"`
	EndEvents    int            `json:"end_events"`
	ParseNS      int64          `json:"parse_ns"`
	WriteNS      int64          `json:"write_ns"`
	WallNS       int64          `json:"wall_ns"`
	Types        map[string]int `json:"types"`
}
type captureTrace struct {
	logger.Interface
	mu     sync.Mutex
	counts map[string]int
}

func (l *captureTrace) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	fields := strings.Fields(sql)
	if len(fields) > 0 {
		l.mu.Lock()
		l.counts[fields[0]]++
		l.mu.Unlock()
	}
}

func captureDecode(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	body, err := io.ReadAll(compressed)
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage
		Error  json.RawMessage
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if len(envelope.Result) == 0 || (len(envelope.Error) > 0 && string(envelope.Error) != "null") {
		return fmt.Errorf("retained envelope missing result")
	}
	return tmjson.Unmarshal(envelope.Result, target)
}

// Uses the exact taxindexer codec registrations without constructing an RPC
// client or a keyring. No network request is available to this test pipeline.
func captureCodec() (*client.ChainClient, map[string][]parsers.MessageParser, error) {
	custom := map[string]sdk.Msg{}
	for _, registry := range []map[string]sdk.Msg{tax.GaiaLiquidMsgTypes(), tax.LegacyLSMMsgTypes(), tax.IBCChannelV2MsgTypes(), tax.TokenFactoryMsgTypes(), tax.ProviderMsgTypes(), tax.IBCClientV2MsgTypes(), tax.LiquidityMsgTypes()} {
		for k, v := range registry {
			custom[k] = v
		}
	}
	basics := append(append([]module.AppModuleBasic{}, client.DefaultModuleBasics...), ica.AppModuleBasic{})
	codec, err := client.MakeCodec(basics, custom)
	if err != nil {
		return nil, nil, err
	}
	registry := map[string][]parsers.MessageParser{}
	for _, url := range tax.MessageTypeURLs {
		registry[url] = []parsers.MessageParser{&tax.Parser{ID: "tax:" + url}}
	}
	return &client.ChainClient{Codec: codec}, registry, nil
}

func TestRetained2022CapturePostgres(t *testing.T) {
	root := os.Getenv("COSMOS_RETAINED_CAPTURE_DIR")
	if root == "" {
		t.Skip("set private retained capture directory")
	}
	// Library logs can include payload identities. Disable all application logs;
	// raw failure details, if any, stay in a private file, not in test output.
	prior := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.Disabled)
	defer zerolog.SetGlobalLevel(prior)
	config.SetChainConfig("cosmos")
	clean, database, err := nativepg.Open()
	require.NoError(t, err)
	defer clean()
	connection, err := database.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(12)
	connection.SetMaxIdleConns(12)
	require.NoError(t, indexdb.MigrateModels(database))
	require.NoError(t, database.AutoMigrate(&tax.TaxableEvent{}))
	require.NoError(t, database.Create(&models.Chain{ID: 1, ChainID: "cosmoshub-4", Name: "Cosmos Hub"}).Error)
	urls := append([]string{}, tax.MessageTypeURLs...)
	sort.Strings(urls)
	trackers := map[string]models.MessageParser{}
	for i, url := range urls {
		p := models.MessageParser{ID: uint(i + 1), Identifier: "tax:" + url}
		require.NoError(t, database.Create(&p).Error)
		trackers[p.Identifier] = p
	}
	seedFingerprint, seedCounts := saveLaneSnapshot(t, database)
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	require.NoError(t, err)
	var manifest captureManifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	filters, err := os.ReadFile(filepath.Join(filepath.Dir(root), "block-filters.json"))
	require.NoError(t, err)
	beginFilters, beginRolling, endFilters, endRolling, messageFilters, err := config.ParseJSONFilterConfig(filters)
	require.NoError(t, err)
	beginRegistry := filter.StaticBlockEventFilterRegistry{BlockEventFilters: beginFilters, RollingWindowEventFilters: beginRolling}
	endRegistry := filter.StaticBlockEventFilterRegistry{BlockEventFilters: endFilters, RollingWindowEventFilters: endRolling}
	trace := &captureTrace{Interface: logger.Default.LogMode(logger.Silent), counts: map[string]int{}}
	measured := database.Session(&gorm.Session{Logger: trace})
	conf := saveLaneConfig()
	mode := os.Getenv("COSMOS_RETAINED_CAPTURE_SHARD")
	if mode == "" {
		mode = "all"
	}
	type result struct {
		stats captureStats
		err   error
	}
	passes := 1
	if os.Getenv("COSMOS_RETAINED_REPLAY") == "1" {
		passes = 2
	}
	priorFingerprint := ""
	for pass := 0; pass < passes; pass++ {
		results := make(chan result, 3)
		shardCount := 0
		if os.Getenv("COSMOS_RETAINED_SQL_LOG") == "1" {
			require.NoError(t, database.Exec("SELECT 'DBSAVE_REAL_BEGIN'").Error)
		}
		started := time.Now()
		for _, shard := range manifest.Shards {
			shard := shard
			if mode != "all" && mode != fmt.Sprint(shard.Shard) {
				continue
			}
			shardCount++
			run := func() {
				stats := captureStats{Shard: shard.Shard, Types: map[string]int{}}
				client, registry, err := captureCodec()
				if err != nil {
					results <- result{stats, err}
					return
				}
				begin := time.Now()
				for _, selected := range shard.Selected {
					parsing := time.Now()
					var block ctypes.ResultBlock
					var blockResults rpc.CustomBlockResults
					dir := filepath.Join(root, fmt.Sprint(shard.Shard))
					if err = captureDecode(filepath.Join(dir, fmt.Sprintf("%d-block.raw.gz", selected.Height)), &block); err != nil {
						break
					}
					if err = captureDecode(filepath.Join(dir, fmt.Sprintf("%d-block_results.raw.gz", selected.Height)), &blockResults); err != nil {
						break
					}
					normalized, normalizeErr := core.NormalizeCustomBlockResults(&blockResults)
					if normalizeErr != nil {
						err = normalizeErr
						break
					}
					blockResults = *normalized
					var model models.Block
					model, err = core.ProcessBlock(&block, &blockResults, 1)
					if err != nil {
						break
					}
					var eventWrapper *indexdb.BlockDBWrapper
					eventWrapper, err = core.ProcessRPCBlockResults(conf, model, &blockResults, nil, nil)
					if err != nil {
						break
					}
					eventWrapper.BeginBlockEvents, err = core.FilterRPCBlockEvents(eventWrapper.BeginBlockEvents, beginRegistry)
					if err != nil {
						break
					}
					eventWrapper.EndBlockEvents, err = core.FilterRPCBlockEvents(eventWrapper.EndBlockEvents, endRegistry)
					if err != nil {
						break
					}
					var txs []indexdb.TxDBWrapper
					txs, _, err = core.ProcessRPCBlockByHeightTXs(&conf, database, client, messageFilters, nil, &block, &blockResults, registry)
					if err != nil {
						break
					}
					stats.ParseNS += time.Since(parsing).Nanoseconds()
					stats.Blocks++
					stats.Txs += len(txs)
					stats.BeginEvents += len(eventWrapper.BeginBlockEvents)
					stats.EndEvents += len(eventWrapper.EndBlockEvents)
					for _, tx := range txs {
						for _, message := range tx.Messages {
							stats.Messages++
							stats.Types[message.Message.MessageType.MessageType]++
							if message.Message.Opaque {
								stats.Opaque++
							}
							for _, data := range message.MessageParsedDatasets {
								if data.Error != nil {
									stats.ParserErrors++
								}
							}
							for _, event := range message.MessageEvents {
								stats.Events++
								stats.Attributes += len(event.Attributes)
							}
						}
					}
					writing := time.Now()
					// Same per-block lane ordering for both binaries. No message/parser reorder.
					var indexed *indexdb.BlockDBWrapper
					indexed, err = indexdb.IndexBlockEvents(measured, false, eventWrapper, "")
					if err != nil {
						break
					}
					if err = indexdb.IndexCustomBlockEvents(conf, measured, false, indexed, "", nil, nil); err != nil {
						break
					}
					_, _, err = indexdb.IndexBlockData(measured, model, txs, conf, trackers)
					// DoDBUpdates makes one unconditional final reattempt after a
					// non-transient failure, including a cold concurrent denom insert race.
					if err != nil {
						_, _, err = indexdb.IndexBlockData(measured, model, txs, conf, trackers)
					}
					if err != nil {
						break
					}
					stats.WriteNS += time.Since(writing).Nanoseconds()
				}
				stats.WallNS = time.Since(begin).Nanoseconds()
				results <- result{stats, err}
			}
			if mode == "all" {
				go run()
			} else {
				run()
			}
		}
		require.Positive(t, shardCount)
		stats := []captureStats{}
		for i := 0; i < shardCount; i++ {
			r := <-results
			if r.err != nil {
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(root), "parse-write-failure.private.txt"), []byte(r.err.Error()), 0600))
				t.Fatal("retained parser/writer failure, private detail retained")
			}
			stats = append(stats, r.stats)
		}
		elapsed := time.Since(started).Nanoseconds()
		if os.Getenv("COSMOS_RETAINED_SQL_LOG") == "1" {
			require.NoError(t, database.Exec("SELECT 'DBSAVE_REAL_END'").Error)
		}
		sort.Slice(stats, func(i, j int) bool { return stats[i].Shard < stats[j].Shard })
		fingerprint, counts := saveLaneSnapshot(t, database)
		if pass > 0 {
			require.Equal(t, priorFingerprint, fingerprint, "identical real capture replay must preserve all table fields and multiplicity")
		}
		priorFingerprint = fingerprint
		resultJSON, err := json.Marshal(map[string]any{"mode": mode, "elapsed_ns": elapsed, "stats": stats, "statements": trace.counts, "fingerprint": fingerprint, "rows": counts, "seed_fingerprint": seedFingerprint, "seed_rows": seedCounts})
		require.NoError(t, err)
		t.Logf("REAL_RESULT pass=%d %s", pass, resultJSON)
	}
}
