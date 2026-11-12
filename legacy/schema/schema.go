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

var roleName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// SQL returns the migration for the given importer role. It is the whole
// change: the schema, its tables and the role.
func SQL(role string) (string, error) {
	if !roleName.MatchString(role) {
		return "", fmt.Errorf("importer role name must be lower case letters, digits and underscores")
	}
	return strings.ReplaceAll(script, "{{role}}", role), nil
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

// writtenOnce lists the tables that hold what the chain recorded. The
// importer may add rows to them and nothing more.
var writtenOnce = []string{"blocks", "tx_inclusions", "taxable_events", "genesis_files", "genesis_accounts",
	"genesis_delegations", "genesis_unbondings", "genesis_validators", "genesis_withdraw_addresses"}

// Confined reports every way the connected role could read or change
// something outside the legacy schema, or rewrite what the schema stores
// once. An empty result means the connection can add legacy rows and do
// nothing else. Every legacy writer calls it before its first write and
// refuses to run on any finding.
func Confined(db *gorm.DB) ([]string, error) {
	var findings []string
	count := func(problem, query string, args ...any) error {
		var n int64
		if err := db.Raw(query, args...).Scan(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			findings = append(findings, fmt.Sprintf("%s: %d", problem, n))
		}
		return nil
	}
	const outside = `n.nspname <> 'legacy' AND n.nspname <> 'pg_catalog' AND n.nspname <> 'information_schema'
		AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'`
	checks := []struct{ problem, query string }{
		{"role is a superuser or may create roles or databases, replicate or bypass row security",
			`SELECT count(*) FROM pg_roles WHERE rolname = current_user
			 AND (rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls)`},
		{"role is a member of other roles",
			`SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member WHERE r.rolname = current_user`},
		{"role owns tables, views or sequences",
			`SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner WHERE r.rolname = current_user`},
		{"role owns schemas",
			`SELECT count(*) FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner WHERE r.rolname = current_user`},
		{"role owns databases",
			`SELECT count(*) FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE r.rolname = current_user`},
		{"role may create schemas in this database",
			`SELECT count(*) WHERE has_database_privilege(current_database(), 'CREATE')`},
		{"schemas the role may create objects in",
			`SELECT count(*) FROM pg_namespace n WHERE n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
			 AND has_schema_privilege(n.oid, 'CREATE')`},
		{"tables and views outside legacy the role may read or change",
			`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE ` + outside + ` AND CASE WHEN c.relkind IN ('r', 'p', 'v', 'm', 'f')
			 THEN has_table_privilege(c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER') ELSE false END`},
		{"sequences outside legacy the role may use",
			`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE ` + outside + ` AND CASE WHEN c.relkind = 'S' THEN has_sequence_privilege(c.oid, 'USAGE, SELECT, UPDATE') ELSE false END`},
		{"legacy tables the role may truncate, alter through triggers or reference",
			`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'legacy' AND CASE WHEN c.relkind IN ('r', 'p')
			 THEN has_table_privilege(c.oid, 'TRUNCATE, REFERENCES, TRIGGER') ELSE false END`},
	}
	for _, check := range checks {
		if err := count(check.problem, check.query); err != nil {
			return nil, err
		}
	}
	if err := count("written once tables the role may update or delete from",
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'legacy' AND c.relname IN ? AND has_table_privilege(c.oid, 'UPDATE, DELETE')`, writtenOnce); err != nil {
		return nil, err
	}
	var present int64
	if err := db.Raw(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'legacy' AND c.relname IN ? AND has_table_privilege(c.oid, 'SELECT')
		AND has_table_privilege(c.oid, 'INSERT')`, writtenOnce).Scan(&present).Error; err != nil {
		return nil, err
	}
	if present != int64(len(writtenOnce)) {
		findings = append(findings, "legacy tables are missing or the role may not add rows to them")
	}
	return findings, nil
}
