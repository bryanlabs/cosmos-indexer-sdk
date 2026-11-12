// Package schema holds the tables for Cosmos Hub 1 to 3 history and the role
// that writes them.
//
// The old chains get their own Postgres schema, "legacy". The serving tables
// identify a transaction by hash alone and a tax row by a message id, and
// neither can hold a ledger whose heights restart three times. Rather than
// change those tables, the importer writes only here, connected as a role
// that has no right on anything else. "The import cannot change a row people
// already see" is then a property of the database, checked by Confined
// before any write, and not of the importer's code.
package schema

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"gorm.io/gorm"
)

// DefaultRole is the importer role the migration creates.
const DefaultRole = "legacy_importer"

//go:embed schema.sql
var script string

//go:embed confined.sql
var confinedQuery string

var roleName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// writtenOnce lists the tables that hold what the chain recorded. The
// importer may add rows to them and nothing more.
var writtenOnce = []string{"blocks", "tx_inclusions", "taxable_events", "genesis_files", "genesis_accounts",
	"genesis_delegations", "genesis_unbondings", "genesis_validators", "genesis_withdraw_addresses"}

// confinedSQL is one query that returns a line for every way the named role
// could reach outside the legacy schema or rewrite what the schema stores
// once. The migration runs it before it commits and every writer runs it
// before its first write, so both apply the same test.
func confinedSQL(role string) (string, error) {
	if !roleName.MatchString(role) {
		return "", fmt.Errorf("importer role name must be lower case letters, digits and underscores")
	}
	return strings.NewReplacer("{{role}}", role,
		"{{written_once}}", "'"+strings.Join(writtenOnce, "', '")+"'",
		"{{written_once_count}}", fmt.Sprint(len(writtenOnce))).Replace(confinedQuery), nil
}

// SQL returns the migration for the given importer role. It is the whole
// change: the schema, its tables and the role. Its last statement fails,
// which undoes all of it, if the role it leaves is not confined.
func SQL(role string) (string, error) {
	check, err := confinedSQL(role)
	if err != nil {
		return "", err
	}
	return strings.NewReplacer("{{confined}}", strings.TrimSpace(check), "{{role}}", role).Replace(script), nil
}

// Migrate creates the legacy schema and the importer role. It must run as a
// role that may create schemas and roles. Running it again changes nothing.
func Migrate(db *gorm.DB, role string) error {
	text, err := SQL(role)
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error { return tx.Exec(text).Error })
}

// Confined reports every way the connected role could read or change
// something outside the legacy schema, or rewrite what the schema stores
// once. An empty result means the connection can add legacy rows and do
// nothing else. Every legacy writer calls it before its first write and
// refuses to run on any finding.
func Confined(db *gorm.DB) ([]string, error) {
	var who struct{ Current, Session string }
	if err := db.Raw(`SELECT current_user::text AS current, session_user::text AS session`).Scan(&who).Error; err != nil {
		return nil, err
	}
	if who.Current != who.Session {
		return []string{"the connection logged in as one role and switched to another, which it can undo"}, nil
	}
	query, err := confinedSQL(who.Current)
	if err != nil {
		return []string{"the connected role does not have an importer role name"}, nil
	}
	findings := []string{}
	if err := db.Raw(query).Scan(&findings).Error; err != nil {
		return nil, err
	}
	if len(findings) == 0 {
		return nil, nil
	}
	return findings, nil
}
