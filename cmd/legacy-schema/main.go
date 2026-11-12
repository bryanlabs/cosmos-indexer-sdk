// Command legacy-schema prints the migration that creates the legacy schema
// and its importer role, and checks that a connection is confined to it.
//
// It never changes the serving database. --print writes the SQL, which is
// the whole change and is applied there by hand in a reviewed step. --apply
// runs it, and only on a private loopback database whose name starts with
// "legacy_". --check connects as the importer role, reads the catalog and
// reports anything the role could reach outside the schema.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/legacy/schema"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var privateName = regexp.MustCompile(`^legacy_[a-z0-9_]{1,40}$`)

func fail(text string) {
	fmt.Fprintln(os.Stderr, text)
	os.Exit(2)
}

func main() {
	role := flag.String("role", schema.DefaultRole, "importer role name")
	print := flag.Bool("print", false, "write the migration SQL to standard output")
	apply := flag.Bool("apply", false, "run the migration on the private database named by LEGACY_ADMIN_DSN")
	check := flag.Bool("check", false, "report what the role of LEGACY_DSN could reach outside the legacy schema")
	flag.Parse()
	quiet := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	switch {
	case *print && !*apply && !*check:
		text, err := schema.SQL(*role)
		if err != nil {
			fail(err.Error())
		}
		fmt.Print(text)
	case *apply && !*print && !*check:
		dsn := os.Getenv("LEGACY_ADMIN_DSN")
		fields := map[string]string{}
		for _, part := range strings.Fields(dsn) {
			k, v, _ := strings.Cut(part, "=")
			fields[k] = v
		}
		ip := net.ParseIP(fields["host"])
		if ip == nil || !ip.IsLoopback() || fields["port"] == "" || fields["port"] == "15432" || !privateName.MatchString(fields["dbname"]) {
			fail("refusing a database that is not a private loopback database with a legacy_ name")
		}
		db, err := gorm.Open(postgres.Open(dsn), quiet)
		if err != nil {
			fail("private database unavailable")
		}
		if err := schema.Migrate(db, *role); err != nil {
			fail("migration failed")
		}
		fmt.Println("legacy schema and importer role are in place")
	case *check && !*print && !*apply:
		db, err := gorm.Open(postgres.Open(os.Getenv("LEGACY_DSN")), quiet)
		if err != nil {
			fail("database unavailable")
		}
		findings, err := schema.Confined(db)
		if err != nil {
			fail("catalog not readable")
		}
		for _, finding := range findings {
			fmt.Println(finding)
		}
		if len(findings) > 0 {
			os.Exit(1)
		}
		fmt.Println("the role can add legacy rows and reach nothing else")
	default:
		fail("choose one of --print, --apply or --check")
	}
}
