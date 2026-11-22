package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	dbpkg "github.com/DefiantLabs/cosmos-indexer/db"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
	"gorm.io/gorm"
)

// The message-bodies loader writes recovered message bytes into
// messages.message_bytes where none are stored, so replays can parse those
// messages again. Each plan line carries the whole archive transaction; a body
// is written only when that transaction hashes to the stored txes.hash and its
// message at the stored message_index has the stored type.

type mbRow struct {
	MessageID    uint
	MessageIndex int
	TxHash       string
	TypeURL      string
	Body         []byte
}

type mbPart struct {
	N           int
	First, Last uint
	Rows        []mbRow
	Sum         string
}

// mbVerdict counts, per part, the rows to write and every reason to refuse.
type mbVerdict struct {
	Rows, Todo, Written                          int
	Missing, Hash, Index, Type, Opaque, Conflict int
	// Private counts rows in the transaction of the private commission
	// profile's message, whose proof hashes the bytes and xmin of every
	// message in that transaction.
	Private int
}

func (v mbVerdict) failures() int {
	return v.Missing + v.Hash + v.Index + v.Type + v.Opaque + v.Conflict + v.Private
}

// parseMbPlanLine reads message id, message index, tx hash and hex tx bytes,
// and takes the body from the transaction itself.
func parseMbPlanLine(line string) (mbRow, error) {
	f := strings.Split(line, "\t")
	if len(f) != 4 {
		return mbRow{}, fmt.Errorf("%d columns, want 4", len(f))
	}
	id, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil || id == 0 {
		return mbRow{}, fmt.Errorf("bad message id")
	}
	idx, err := strconv.Atoi(f[1])
	if err != nil || idx < 0 {
		return mbRow{}, fmt.Errorf("bad message index")
	}
	raw, err := hex.DecodeString(f[3])
	if err != nil || len(raw) == 0 {
		return mbRow{}, fmt.Errorf("bad tx hex")
	}
	sum := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), f[2]) {
		return mbRow{}, fmt.Errorf("tx bytes do not hash to the line's tx hash")
	}
	var txRaw sdktx.TxRaw
	if err := txRaw.Unmarshal(raw); err != nil {
		return mbRow{}, fmt.Errorf("tx bytes: %w", err)
	}
	var body sdktx.TxBody
	if err := body.Unmarshal(txRaw.BodyBytes); err != nil {
		return mbRow{}, fmt.Errorf("tx body: %w", err)
	}
	if idx >= len(body.Messages) || len(body.Messages[idx].Value) == 0 {
		return mbRow{}, fmt.Errorf("tx has no message %d", idx)
	}
	return mbRow{MessageID: uint(id), MessageIndex: idx, TxHash: strings.ToUpper(f[2]), TypeURL: body.Messages[idx].TypeUrl, Body: body.Messages[idx].Value}, nil
}

// readRecoveredBodies hashes the plan file and parses every line, refusing a
// repeated message, and returns the rows sorted by message id with each
// row's line text.
func readRecoveredBodies(path string) (string, []mbRow, error) {
	sum, rows, _, err := readRecoveredLines(path)
	return sum, rows, err
}

func readRecoveredLines(path string) (string, []mbRow, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, nil, err
	}
	whole := sha256.Sum256(data)
	type line struct {
		row  mbRow
		text string
	}
	var lines []line
	seen := map[uint]bool{}
	for n, text := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		r, err := parseMbPlanLine(text)
		if err != nil {
			return "", nil, nil, fmt.Errorf("plan line %d: %w", n+1, err)
		}
		if seen[r.MessageID] {
			return "", nil, nil, fmt.Errorf("plan line %d: message %d twice", n+1, r.MessageID)
		}
		seen[r.MessageID] = true
		lines = append(lines, line{r, text})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].row.MessageID < lines[j].row.MessageID })
	rows := make([]mbRow, len(lines))
	texts := make([]string, len(lines))
	for i, l := range lines {
		rows[i], texts[i] = l.row, l.text
	}
	return hex.EncodeToString(whole[:]), rows, texts, nil
}

// readMessageBodiesPlan splits the plan's rows into parts, each with the
// sha256 of its own lines.
func readMessageBodiesPlan(path string, perPart int) (string, []mbPart, error) {
	sum, rows, texts, err := readRecoveredLines(path)
	if err != nil {
		return "", nil, err
	}
	var parts []mbPart
	for start := 0; start < len(rows); start += perPart {
		end := min(start+perPart, len(rows))
		h := sha256.New()
		for _, t := range texts[start:end] {
			h.Write([]byte(t + "\n"))
		}
		parts = append(parts, mbPart{N: len(parts) + 1, First: rows[start].MessageID, Last: rows[end-1].MessageID, Rows: rows[start:end], Sum: hex.EncodeToString(h.Sum(nil))})
	}
	return sum, parts, nil
}

// refuseRecoveredBody names why a recovered body may not stand for a stored
// message, or returns "" when it may: the archive transaction must hash to
// the stored tx hash, and its message at the stored message index must have
// the stored type, which must be a MsgExec or an IBC refund lifecycle message
// (timeout or acknowledgement), the types replays cannot rebuild from events.
func refuseRecoveredBody(r mbRow, storedHash string, storedIndex int, storedType string) string {
	switch {
	case !strings.EqualFold(storedHash, r.TxHash):
		return "hash"
	case storedIndex != r.MessageIndex:
		return "index"
	case r.TypeURL != storedType || !recoverableBodyType(storedType):
		return "type"
	}
	return ""
}

func recoverableBodyType(typeURL string) bool {
	return typeURL == typeURLAuthzExec || slices.Contains(refundLifecycleTypeURLs, typeURL)
}

func mbIDs(rows []mbRow) []uint {
	ids := make([]uint, len(rows))
	for i, r := range rows {
		ids[i] = r.MessageID
	}
	return ids
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int) int {
	if n == nil {
		return -1
	}
	return *n
}

type mbStored struct {
	ID           uint
	Found        bool
	MessageIndex *int
	MessageType  *string
	MessageBytes []byte
	Opaque       *bool
	Hash         *string
	PrivateTx    bool
}

// checkMbPart compares every row with what is stored now. It only reads.
func checkMbPart(tx *gorm.DB, p mbPart) (mbVerdict, []mbRow, error) {
	var stored []mbStored
	err := tx.Raw(`SELECT u.id, m.id IS NOT NULL AS found, m.message_index, mt.message_type, m.message_bytes, m.opaque, t.hash,
 coalesce(m.tx_id = (SELECT pm.tx_id FROM messages pm WHERE pm.id = ?), false) AS private_tx
FROM unnest(?::bigint[]) u(id)
LEFT JOIN LATERAL (SELECT m.id, m.tx_id, m.message_type_id, m.message_index, m.message_bytes, m.opaque FROM messages m WHERE m.id = u.id OFFSET 0) m ON true
LEFT JOIN LATERAL (SELECT mt.message_type FROM message_types mt WHERE mt.id = m.message_type_id OFFSET 0) mt ON true
LEFT JOIN LATERAL (SELECT t.hash FROM txes t WHERE t.id = m.tx_id OFFSET 0) t ON true`, privateProfileMessageID, idArray(mbIDs(p.Rows))).Scan(&stored).Error
	if err != nil {
		return mbVerdict{}, nil, err
	}
	byID := map[uint]mbStored{}
	for _, s := range stored {
		byID[s.ID] = s
	}
	v := mbVerdict{Rows: len(p.Rows)}
	var todo []mbRow
	for _, r := range p.Rows {
		s := byID[r.MessageID]
		reason := ""
		if s.Found {
			reason = refuseRecoveredBody(r, deref(s.Hash), derefInt(s.MessageIndex), deref(s.MessageType))
		}
		switch {
		case !s.Found:
			v.Missing++
		case s.PrivateTx:
			v.Private++
		case reason == "hash":
			v.Hash++
		case reason == "index":
			v.Index++
		case reason == "type":
			v.Type++
		case s.Opaque != nil && *s.Opaque:
			v.Opaque++
		case s.MessageBytes == nil:
			v.Todo++
			todo = append(todo, r)
		case bytes.Equal(s.MessageBytes, r.Body):
			v.Written++
		default:
			v.Conflict++
		}
	}
	return v, todo, nil
}

// createBackup creates the backup file, never over an existing one, and
// syncs its directory so the file's entry survives a crash along with the
// lines synced into it.
func createBackup(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = dir.Sync()
		if cerr := dir.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sync backup directory: %w", err)
	}
	return f, nil
}

// loadMbPart writes one part in its own transaction. Its messages rows are
// locked FOR UPDATE in id order, as applyPlans locks them, which keeps a
// replay of the same messages out; the advisory lock it shares with the
// withdraw-address loader only keeps loader runs apart.
// With apply false it rolls back after the write and the read-back. With
// apply it first appends every row it will write to backup, as id and the
// NULL it replaces, and syncs that file.
func loadMbPart(db *gorm.DB, p mbPart, apply bool, backup *os.File) (v mbVerdict, written int, err error) {
	errRollback := errors.New("dry run")
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout = '900s'; SET LOCAL lock_timeout = '10s'; SET LOCAL jit = off").Error; err != nil {
			return err
		}
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", waLoadLockKey).Error; err != nil {
			return err
		}
		ids := mbIDs(p.Rows)
		var locked []uint
		if err := tx.Raw("SELECT id FROM messages WHERE id IN ? ORDER BY id FOR UPDATE", ids).Scan(&locked).Error; err != nil {
			return err
		}
		var todo []mbRow
		if v, todo, err = checkMbPart(tx, p); err != nil {
			return err
		}
		if v.failures() != 0 || len(locked) != len(ids) {
			return fmt.Errorf("part %d refused: %+v", p.N, v)
		}
		if len(todo) == 0 {
			return errRollback
		}
		if apply {
			if backup == nil {
				return errors.New("apply needs a backup file")
			}
			w := bufio.NewWriter(backup)
			for _, r := range todo {
				fmt.Fprintf(w, "%d\tNULL\n", r.MessageID)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if err := backup.Sync(); err != nil {
				return err
			}
		}
		hexes := make([]string, len(todo))
		for i, r := range todo {
			hexes[i] = hex.EncodeToString(r.Body)
		}
		res := tx.Exec(`UPDATE messages m SET message_bytes = decode(v.body, 'hex')
FROM unnest(?::bigint[], ?::text[]) v(id, body)
WHERE m.id = v.id AND m.message_bytes IS NULL`, idArray(mbIDs(todo)), "{"+strings.Join(hexes, ",")+"}")
		if res.Error != nil {
			return res.Error
		}
		written = int(res.RowsAffected)
		after, _, err := checkMbPart(tx, p)
		if err != nil {
			return err
		}
		if written != len(todo) || after.Written != v.Written+len(todo) || after.failures() != 0 || after.Todo != 0 {
			return fmt.Errorf("part %d post check: wrote %d of %d, read back %+v", p.N, written, len(todo), after)
		}
		if !apply {
			return errRollback
		}
		return nil
	})
	if errors.Is(err, errRollback) {
		err = nil
	}
	return v, written, err
}

func runMessageBodiesLoad(args []string) int {
	fs := flag.NewFlagSet("message-bodies-load", flag.ExitOnError)
	planPath := fs.String("plan", "", "plan file: message id, message index, tx hash, hex tx bytes")
	perPart := fs.Int("part-messages", 500, "messages per part, each written in its own transaction")
	check := fs.Bool("check", false, "read-only check of every part on a read-only connection; nothing is locked or written")
	apply := fs.Bool("apply", false, "commit each part (requires --sha and --backup)")
	want := fs.String("sha", "", "sha256 of the plan file the dry run reported; apply refuses any other file")
	backupPath := fs.String("backup", "", "new private file that records each written row as id and NULL before it is written")
	_ = fs.Parse(args)
	if *planPath == "" || *perPart < 1 || *perPart > 5000 || (*apply && (*check || *want == "" || *backupPath == "")) {
		fatal("message-bodies-load needs --plan, 1 to 5000 --part-messages, and --apply only with --sha and --backup and without --check")
	}
	sum, parts, err := readMessageBodiesPlan(*planPath, *perPart)
	if err != nil {
		fatal(err.Error())
	}
	fmt.Printf("plan sha256 %s, %d parts\n", sum, len(parts))
	if *apply && sum != *want {
		fatal("the plan file's sha256 is not the one --sha names")
	}
	params := []string{"application_name=message-bodies-load"}
	if *check {
		params = append(params, "default_transaction_read_only=on")
	}
	db, err := dbpkg.PostgresDbConnect(env("DB_HOST"), envDefault("DB_PORT", "5432"), env("DB_NAME"), env("DB_USER"), os.Getenv("DB_PASS"), "silent", params...)
	if err != nil {
		fatal(err.Error())
	}
	sqlDB, err := db.DB()
	if err != nil {
		fatal(err.Error())
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	if *check {
		return runMbCheck(db, parts)
	}
	var backup *os.File
	if *apply {
		if backup, err = createBackup(*backupPath); err != nil {
			fatal(err.Error())
		}
		defer backup.Close()
	}
	total := 0
	for _, p := range parts {
		start := time.Now()
		v, n, err := loadMbPart(db, p, *apply, backup)
		if err != nil {
			fatal(err.Error())
		}
		total += n
		fmt.Printf("part %d messages %d..%d sha256 %s: %d rows, %d already written, %d written, %.1fs\n",
			p.N, p.First, p.Last, p.Sum, v.Rows, v.Written, n, time.Since(start).Seconds())
	}
	if *apply {
		fmt.Printf("committed %d bodies\n", total)
	} else {
		fmt.Printf("dry run: %d bodies would be written, every part rolled back\n", total)
	}
	return 0
}

func runMbCheck(db *gorm.DB, parts []mbPart) int {
	failed, err := checkMbParts(db, parts)
	if err != nil {
		fatal(err.Error())
	}
	if failed != 0 {
		fmt.Printf("%d parts would be refused\n", failed)
		return 1
	}
	fmt.Println("every part passes")
	return 0
}

// checkMbParts checks every part in its own read-only transaction and
// refuses to run on a connection that is not read only.
func checkMbParts(db *gorm.DB, parts []mbPart) (failed int, err error) {
	for _, p := range parts {
		start := time.Now()
		var v mbVerdict
		err := db.Transaction(func(tx *gorm.DB) error {
			var ro string
			if err := tx.Raw("SHOW transaction_read_only").Scan(&ro).Error; err != nil || ro != "on" {
				return fmt.Errorf("check transaction is not read only (%q, %v)", ro, err)
			}
			if err := tx.Exec("SET LOCAL statement_timeout = '900s'; SET LOCAL jit = off").Error; err != nil {
				return err
			}
			var err error
			v, _, err = checkMbPart(tx, p)
			return err
		})
		if err != nil {
			return failed, err
		}
		if v.failures() != 0 {
			failed++
		}
		fmt.Printf("part %d messages %d..%d sha256 %s: %d rows, %d to write, %d already written, refused missing %d hash %d index %d type %d opaque %d conflict %d private %d, %.1fs\n",
			p.N, p.First, p.Last, p.Sum, v.Rows, v.Todo, v.Written, v.Missing, v.Hash, v.Index, v.Type, v.Opaque, v.Conflict, v.Private, time.Since(start).Seconds())
	}
	return failed, nil
}
