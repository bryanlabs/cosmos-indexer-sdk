// Command legacy-genesis loads the opening state of cosmoshub-1, 2, 3 or 4
// from its genesis file into the legacy schema. It checks the file against
// a SHA256SUMS list first, makes no network calls besides the database, and
// prints counts only: no address, balance or amount.
//
// The database role in LEGACY_DSN must be confined to the legacy schema;
// the load refuses to start otherwise. --check-only parses and checks the
// file and connects to nothing.
package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/legacy/genesis"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	digits   = regexp.MustCompile(`\d+`)
	safeText = regexp.MustCompile(`^[A-Za-z0-9 _.,:;/()'-]{1,400}$`)
)

func safe(err error) string {
	text := digits.ReplaceAllString(err.Error(), "N")
	if !safeText.MatchString(text) {
		return "error text withheld"
	}
	return text
}

func fail(text string) {
	fmt.Fprintln(os.Stderr, text)
	os.Exit(2)
}

func main() {
	path := flag.String("file", "", "genesis file, plain or .gz")
	sums := flag.String("sums", "", "SHA256SUMS file that lists the genesis file by name")
	checkOnly := flag.Bool("check-only", false, "parse and check the file, write nothing")
	flag.Parse()
	if *path == "" || *sums == "" {
		fail("--file and --sums are required")
	}
	list, err := os.ReadFile(*sums)
	if err != nil {
		fail("checksum list unavailable")
	}
	want := ""
	for _, line := range strings.Split(string(list), "\n") {
		if parts := strings.Fields(line); len(parts) == 2 && strings.TrimPrefix(parts[1], "*") == filepath.Base(*path) {
			want = parts[0]
		}
	}
	if want == "" {
		fail("the checksum list does not name this file")
	}
	source, err := os.Open(*path)
	if err != nil {
		fail("genesis file unavailable")
	}
	defer source.Close()
	// The checksum list covers the file as stored, so a compressed file is
	// checked compressed and parsed decompressed.
	stored := genesis.NewChecksum(source)
	var content io.Reader = stored
	if strings.HasSuffix(*path, ".gz") {
		if content, err = gzip.NewReader(stored); err != nil {
			fail("genesis file is not gzip")
		}
	}
	file, err := genesis.Read(content)
	if err != nil {
		fail(safe(err))
	}
	if stored.Sum() != want {
		fail("genesis file does not match its checksum")
	}
	file.SHA256 = want
	counts := file.Counts()
	fmt.Printf("%s: checksum matches, accounts %d, delegations %d, unbonding entries %d, validators %d, withdraw addresses %d, genesis transactions %d\n",
		file.Chain, counts.Accounts, counts.Delegations, counts.Unbondings, counts.Validators, counts.Withdraws, counts.GenesisTxs)
	for _, check := range file.Checks {
		fmt.Println("  passed: " + check)
	}
	if *checkOnly {
		return
	}
	dsn := os.Getenv("LEGACY_DSN")
	if strings.Contains(dsn, "port=15432") || strings.Contains(dsn, ":15432") {
		fail("refusing port 15432")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		fail("database unavailable")
	}
	result, err := genesis.Load(db, file)
	if err != nil {
		fail(safe(err))
	}
	state := "loaded"
	if result.AlreadyLoaded {
		state = "already loaded, nothing written"
	}
	if result.Stored != result.File {
		fail("stored row counts differ from the file")
	}
	fmt.Printf("%s: %s; every stored row read back equal to the file: accounts %d, delegations %d, unbonding entries %d, validators %d, withdraw addresses %d\n",
		file.Chain, state, result.Stored.Accounts, result.Stored.Delegations, result.Stored.Unbondings, result.Stored.Validators, result.Stored.Withdraws)
}
