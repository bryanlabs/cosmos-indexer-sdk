package taxapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateReadinessCauseExactRetainedInput(t *testing.T) {
	root := os.Getenv("READINESS_CAUSE_ROOT")
	attempt := os.Getenv("READINESS_CAUSE_ATTEMPT")
	if attempt == "" {
		attempt = "current"
	}
	if root == "" {
		t.Fatal("private artifact root required")
	}
	b, err := os.ReadFile(filepath.Join(root, "failure-response.private.json"))
	if err != nil {
		t.Fatal("input read failed")
	}
	var rows []struct {
		Version, Chain, Build string
		Evidence              struct {
			Failure *CommissionReadFailure `json:"ReadFailure"`
		} `json:"evidence_json"`
	}
	if json.Unmarshal(b, &rows) != nil {
		t.Fatal("input decode failed")
	}
	var f *CommissionReadFailure
	for _, r := range rows {
		if r.Evidence.Failure != nil && r.Evidence.Failure.PhaseElapsedMillis == 19 {
			f = r.Evidence.Failure
		}
	}
	if f == nil {
		t.Fatal("exact retained failure absent")
	}
	b, err = os.ReadFile("/Users/danb/.cache/commission-acceleration/prepared-20261114t1500/original-job.private.json")
	if err != nil {
		t.Fatal("config read failed")
	}
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct{ Name, Value string }
					}
				}
			}
		}
	}
	if json.Unmarshal(b, &job) != nil {
		t.Fatal("config decode failed")
	}
	e := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Containers[0].Env {
		e[v.Name] = v.Value
	}
	dsn := fmt.Sprintf("host=127.0.0.1 port=15438 user=%s password=%s dbname=%s sslmode=disable default_transaction_read_only=on statement_timeout=5000 lock_timeout=500 application_name=readiness-cause-ro-20261114", e["DB_USER"], e["DB_PASS"], e["DB_NAME"])
	qlog, err := os.OpenFile(filepath.Join(root, "native-readiness-query-"+attempt+".private.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("exclusive query log failed")
	}
	defer qlog.Close()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.New(log.New(qlog, "", 0), logger.Config{LogLevel: logger.Info, Colorful: false})})
	if err != nil {
		t.Fatal("private DB open failed")
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal("connection failed")
	}
	conn.SetMaxOpenConns(1)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := &Server{db: db.WithContext(ctx), readOnlyLedger: true, privateReadSnapshotTimeout: 20 * time.Second}
	out := map[string]interface{}{"at": time.Now().UTC(), "financial_acceptance": false, "source": "6754a0d7d7986e48e3e3467b2448aae84d2c1b92", "wallet_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(f.Address))), "exact_retained_start": f.Start, "exact_retained_end": f.End}
	started := time.Now()
	err = s.withReadSnapshot(func(reader *Server) error {
		var ro string
		if e := reader.db.Raw("SHOW transaction_read_only").Scan(&ro).Error; e != nil {
			return e
		}
		out["transaction_read_only"] = ro
		if ro != "on" {
			return errors.New("read only guard failed")
		}
		r, e := reader.classificationReadiness(apiMainnet, []string{f.Address}, f.Start, f.End)
		out["classification"] = r
		if e != nil {
			return e
		}
		candidates := reader.db.Table("txes t").Select("t.id").Joins("JOIN blocks b ON b.id=t.block_id").Joins("JOIN chains c ON c.id=b.chain_id").Joins("JOIN (SELECT DISTINCT m.tx_id FROM message_parser_errors mpe JOIN messages m ON m.id=mpe.message_id) parser_scope ON parser_scope.tx_id=t.id").Where("c.chain_id IN ? AND b.time_stamp >= ? AND b.time_stamp < ? AND t.code = 0", chainIDs(apiMainnet), f.Start, f.End)
		selected := walletFewTxScope(reader.db.Table("(? OFFSET 0) t", candidates), []string{f.Address}).Select("t.id")
		type metadata struct {
			ErrorID, MessageID, TxID, Height                           int64
			TxHash, Error, Parser, MessageType, ErrorXmin, MessageXmin string
			Time                                                       time.Time
		}
		var md []metadata
		e = reader.db.Table("message_parser_errors mpe").Select("mpe.id AS error_id,m.id AS message_id,t.id AS tx_id,b.height,t.hash AS tx_hash,mpe.error,mp.identifier AS parser,mt.message_type,mpe.xmin::text AS error_xmin,m.xmin::text AS message_xmin,b.time_stamp AS time").Joins("JOIN messages m ON m.id=mpe.message_id").Joins("JOIN txes t ON t.id=m.tx_id").Joins("JOIN blocks b ON b.id=t.block_id").Joins("JOIN message_parsers mp ON mp.id=mpe.message_parser_id").Joins("JOIN message_types mt ON mt.id=m.message_type_id").Where("m.tx_id IN (?)", selected).Limit(5).Scan(&md).Error
		if e != nil {
			return e
		}
		out["parser_metadata"] = md
		_, e = reader.reportReadiness(apiMainnet, []string{f.Address}, f.Start, f.End)
		return e
	})
	out["elapsed_ms"] = time.Since(started).Milliseconds()
	if err != nil {
		out["raw_error"] = err.Error()
		out["error_type"] = fmt.Sprintf("%T", err)
		out["private_cause"] = privateReadCause(err)
		var ready reportReadinessRequired
		if errors.As(err, &ready) {
			out["readiness_refusal"] = ready.Readiness
		}
	}
	b, _ = json.MarshalIndent(out, "", "  ")
	if os.WriteFile(filepath.Join(root, "native-readiness-"+attempt+".private.json"), b, 0600) != nil {
		t.Fatal("receipt failed")
	}
	if err == nil {
		t.Fatal("stored failure did not reproduce on current source")
	}
}
