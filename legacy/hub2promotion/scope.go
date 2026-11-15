package hub2promotion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

//go:embed scope.sql
var scopeScript string

type Scope struct {
	Schema    string `json:"schema"`
	Loader    string `json:"loader"`
	ViewOwner string `json:"view_owner"`
	LockOwner string `json:"lock_owner"`
	ChainID   string `json:"chain_id,omitempty"`
	RawStage  bool   `json:"raw_stage,omitempty"`
}

func (s Scope) insertTables() []table {
	if !s.RawStage {
		return inserts
	}
	return []table{inserts[6], inserts[8]}
}
func (s Scope) baseRelation(t table) string {
	if s.RawStage {
		return s.Schema + ".raw_" + t.name
	}
	return "legacy." + t.name
}
func (s Scope) lockRelations() string {
	out := relationList(allTables)
	if s.RawStage {
		for _, t := range s.insertTables() {
			out += ", " + s.baseRelation(t)
		}
	}
	return out
}

// Empty means the original Hub2-only profile. Only these two human-approved
// chains can ever be interpolated into the fixed SQL boundary.
func (s Scope) chain() string {
	if s.ChainID == "" {
		return Chain
	}
	return s.ChainID
}

func (s Scope) valid() error {
	if s.chain() != "cosmoshub-1" && s.chain() != "cosmoshub-2" {
		return errors.New("only Hub1 or Hub2 scope is permitted")
	}
	if s.RawStage && s.chain() != "cosmoshub-1" {
		return errors.New("raw stage currently supports Hub1 only")
	}
	seen := map[string]bool{}
	for _, n := range []string{s.Schema, s.Loader, s.ViewOwner, s.LockOwner} {
		if !objectRE.MatchString(n) || seen[n] || !strings.HasPrefix(n, "h2p_") {
			return errors.New("invalid unique scope name")
		}
		seen[n] = true
	}
	return nil
}
func relationList(ts []table) string {
	a := make([]string, len(ts))
	for i, t := range ts {
		a[i] = "legacy." + t.name
	}
	return strings.Join(a, ", ")
}
func (s Scope) SQL() (string, error) {
	if e := s.valid(); e != nil {
		return "", e
	}
	var views strings.Builder
	var rawTables strings.Builder
	var targets []string
	for _, t := range s.insertTables() {
		targets = append(targets, s.baseRelation(t))
		if s.RawStage {
			fmt.Fprintf(&rawTables, "CREATE TABLE %s (LIKE legacy.%s INCLUDING ALL);\nALTER TABLE %s OWNER TO %s;\nREVOKE ALL ON %s FROM PUBLIC;\nGRANT SELECT ON %s TO %s;\nGRANT UPDATE ON %s TO %s;\n", s.baseRelation(t), t.name, s.baseRelation(t), s.ViewOwner, s.baseRelation(t), s.baseRelation(t), s.Loader, s.baseRelation(t), s.LockOwner)
		}
		fmt.Fprintf(&views, "CREATE VIEW %s.%s AS SELECT %s FROM %s WHERE chain_id = '%s' WITH CASCADED CHECK OPTION;\nALTER VIEW %s.%s OWNER TO %s;\nREVOKE ALL ON %s.%s FROM PUBLIC;\nGRANT SELECT, INSERT ON %s.%s TO %s;\n", s.Schema, t.name, quotedColumns(t.columns), s.baseRelation(t), s.chain(), s.Schema, t.name, s.ViewOwner, s.Schema, t.name, s.Schema, t.name, s.Loader)
	}
	if s.RawStage {
		rawTables.WriteString("ALTER TABLE " + s.Schema + ".raw_block_events ADD FOREIGN KEY (chain_id,height) REFERENCES " + s.Schema + ".raw_blocks(chain_id,height);\n")
		for _, t := range s.insertTables() {
			rawTables.WriteString("CREATE TRIGGER written_once_rows BEFORE UPDATE OR DELETE ON " + s.baseRelation(t) + " FOR EACH ROW EXECUTE FUNCTION legacy.refuse_change();\n")
			rawTables.WriteString("CREATE TRIGGER written_once_table BEFORE TRUNCATE ON " + s.baseRelation(t) + " FOR EACH STATEMENT EXECUTE FUNCTION legacy.refuse_change();\n")
		}
	}
	text := strings.NewReplacer("{{scope}}", s.Schema, "{{loader}}", s.Loader, "{{view_owner}}", s.ViewOwner, "{{lock_owner}}", s.LockOwner, "{{all_tables}}", relationList(allTables), "{{insert_tables}}", strings.Join(targets, ", "), "{{views}}", views.String(), "{{view_tables}}", scopeRelations(s)).Replace(scopeScript)
	if s.RawStage {
		text = strings.Replace(text, "GRANT SELECT, INSERT ON ", rawTables.String()+"GRANT SELECT, INSERT ON ", 1)
		text = strings.Replace(text, "LOCK TABLE "+relationList(allTables)+" IN SHARE ROW EXCLUSIVE MODE", "LOCK TABLE "+s.lockRelations()+" IN SHARE ROW EXCLUSIVE MODE", 1)
	}
	return text, nil
}
func scopeRelations(s Scope) string {
	var names []string
	for _, t := range s.insertTables() {
		names = append(names, s.Schema+"."+t.name)
	}
	return strings.Join(names, ", ")
}

func InstallScope(ctx context.Context, db *sql.DB, s Scope) error {
	text, e := s.SQL()
	if e != nil {
		return e
	}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, text); e != nil {
		return e
	}
	if e = CheckScope(ctx, tx, s); e != nil {
		return e
	}
	return tx.Commit()
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// CheckScope examines effective rights, not just explicit ACL entries. PUBLIC
// TEMP is intentionally allowed, not revoked. Every issued object is qualified.
func CheckScope(ctx context.Context, q querier, s Scope) error {
	if e := s.valid(); e != nil {
		return e
	}
	for _, n := range []string{s.Loader, s.ViewOwner, s.LockOwner} {
		var super, createdb, createrole, inherit, repl, bypass, login bool
		e := q.QueryRowContext(ctx, `SELECT rolsuper,rolcreatedb,rolcreaterole,rolinherit,rolreplication,rolbypassrls,rolcanlogin FROM pg_catalog.pg_roles WHERE rolname=$1`, n).Scan(&super, &createdb, &createrole, &inherit, &repl, &bypass, &login)
		if e != nil {
			return e
		}
		if super || createdb || createrole || inherit || repl || bypass || login != (n == s.Loader) {
			return errors.New("unsafe scope role flags")
		}
	}
	var n int
	if e := q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles r ON r.oid=m.roleid OR r.oid=m.member WHERE r.rolname IN ($1,$2,$3)`, s.Loader, s.ViewOwner, s.LockOwner).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("scope role memberships refused")
	}
	rows, e := q.QueryContext(ctx, `SELECT n.nspname,c.relname,c.relkind,c.relowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=$1),
 pg_catalog.has_table_privilege($1,c.oid,'SELECT') OR pg_catalog.has_any_column_privilege($1,c.oid,'SELECT'), pg_catalog.has_table_privilege($1,c.oid,'INSERT') OR pg_catalog.has_any_column_privilege($1,c.oid,'INSERT'),
 pg_catalog.has_table_privilege($1,c.oid,'UPDATE') OR pg_catalog.has_any_column_privilege($1,c.oid,'UPDATE'),
 pg_catalog.has_table_privilege($1,c.oid,'DELETE,TRUNCATE,REFERENCES,TRIGGER') OR pg_catalog.has_any_column_privilege($1,c.oid,'REFERENCES')
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE c.relkind IN ('r','p','v','m','f') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp_%'`, s.Loader)
	if e != nil {
		return e
	}
	defer rows.Close()
	base, views := 0, 0
	for rows.Next() {
		var ns, name, kind string
		var own, sel, ins, upd, other bool
		if e = rows.Scan(&ns, &name, &kind, &own, &sel, &ins, &upd, &other); e != nil {
			return e
		}
		allowedBase := false
		for _, t := range allTables {
			if ns == "legacy" && t.name == name {
				allowedBase = true
			}
		}
		if own || upd || other {
			return errors.New("loader ownership/rewrite privilege")
		}
		if s.RawStage && ns == s.Schema && (name == "raw_blocks" || name == "raw_block_events") {
			allowedBase = true
		}
		allowedView := false
		for _, t := range s.insertTables() {
			if name == t.name {
				allowedView = true
			}
		}
		if ns == s.Schema && allowedView && kind == "v" {
			if !sel || !ins {
				return errors.New("missing view privilege")
			}
			views++
		} else if allowedBase && kind == "r" {
			if !sel || ins {
				return errors.New("base privilege mismatch")
			}
			base++
		} else if sel || ins {
			return errors.New("loader has unrelated relation access")
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	expectedBase := 14
	if s.RawStage {
		expectedBase += 2
	}
	if base != expectedBase || views != len(s.insertTables()) {
		return errors.New("fixed relation set missing")
	}
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname NOT LIKE 'pg_temp_%' AND pg_catalog.has_schema_privilege($1,oid,'CREATE')`, s.Loader).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("loader CREATE privilege")
	}
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_class WHERE CASE WHEN relkind='S' THEN pg_catalog.has_sequence_privilege($1,oid,'USAGE,SELECT,UPDATE') ELSE false END`, s.Loader).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("loader sequence privilege")
	}
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_database WHERE datname=current_database() AND pg_catalog.has_database_privilege($1,oid,'CREATE')`, s.Loader).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("loader database CREATE")
	}
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace ns ON ns.oid=p.pronamespace
 WHERE ns.nspname NOT IN ('pg_catalog','information_schema') AND pg_catalog.has_function_privilege($1,p.oid,'EXECUTE')
 AND NOT (ns.nspname=$2 AND p.proname='lock_legacy' AND p.pronargs=0)
 AND NOT (ns.nspname='legacy' AND p.proname='refuse_change' AND NOT p.prosecdef AND p.prorettype='pg_catalog.trigger'::pg_catalog.regtype)`, s.Loader, s.Schema).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("alternate callable function")
	}
	// No insert trigger/rule may bypass a view CHECK OPTION or rewrite target.
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace ns ON ns.oid=c.relnamespace WHERE ns.nspname IN ('legacy',$1) AND NOT t.tgisinternal AND ((t.tgtype & 4)<>0 OR ns.nspname=$1)
 AND NOT ($2 AND ns.nspname=$1 AND c.relname IN ('raw_blocks','raw_block_events') AND t.tgfoid='legacy.refuse_change()'::pg_catalog.regprocedure AND t.tgenabled='O' AND t.tgnargs=0 AND t.tgqual IS NULL AND t.tgattr=''::pg_catalog.int2vector AND ((t.tgname='written_once_rows' AND t.tgtype=27) OR (t.tgname='written_once_table' AND t.tgtype=34)))`, s.Schema, s.RawStage).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("insert/view trigger refused")
	}
	if s.RawStage {
		if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace ns ON ns.oid=c.relnamespace WHERE ns.nspname=$1 AND c.relname IN ('raw_blocks','raw_block_events') AND NOT t.tgisinternal AND t.tgfoid='legacy.refuse_change()'::pg_catalog.regprocedure AND t.tgenabled='O' AND t.tgnargs=0 AND t.tgqual IS NULL AND t.tgattr=''::pg_catalog.int2vector AND ((t.tgname='written_once_rows' AND t.tgtype=27) OR (t.tgname='written_once_table' AND t.tgtype=34))`, s.Schema).Scan(&n); e != nil {
			return e
		}
		if n != 4 {
			return errors.New("raw written-once trigger protection missing")
		}
	}
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_rewrite r JOIN pg_catalog.pg_class c ON c.oid=r.ev_class JOIN pg_catalog.pg_namespace ns ON ns.oid=c.relnamespace WHERE ns.nspname IN ('legacy',$1) AND NOT (ns.nspname=$1 AND c.relkind='v' AND r.rulename='_RETURN')`, s.Schema).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("alternate rewrite rule")
	}
	normalize := func(v string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) || r == '(' || r == ')' || r == '"' {
				return -1
			}
			return r
		}, v)
	}
	for _, t := range s.insertTables() {
		var def, owner, options string
		e = q.QueryRowContext(ctx, `SELECT pg_catalog.pg_get_viewdef(c.oid,true),r.rolname,c.reloptions::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace ns ON ns.oid=c.relnamespace JOIN pg_catalog.pg_roles r ON r.oid=c.relowner WHERE ns.nspname=$1 AND c.relname=$2 AND c.relkind='v'`, s.Schema, t.name).Scan(&def, &owner, &options)
		if e != nil {
			return e
		}
		fields := strings.Split(t.columns, ",")
		for i, c := range fields {
			if c == "timestamp" {
				c = `"timestamp"`
			}
			fields[i] = c
		}
		expected := "SELECT " + strings.Join(fields, ",") + " FROM " + s.baseRelation(t) + " WHERE chain_id = '" + s.chain() + "'::text;"
		if normalize(def) != normalize(expected) || owner != s.ViewOwner || options != "{check_option=cascaded}" {
			return fmt.Errorf("view definition/owner/check-option drift: %s", t.name)
		}
	}
	// Check the exact helper, rather than admitting an arbitrary SECURITY DEFINER.
	var body, owner, lang, config string
	var sec bool
	var nargs int
	e = q.QueryRowContext(ctx, `SELECT p.prosrc,r.rolname,l.lanname,p.proconfig::text,p.prosecdef,p.pronargs FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace JOIN pg_catalog.pg_roles r ON r.oid=p.proowner JOIN pg_catalog.pg_language l ON l.oid=p.prolang WHERE n.nspname=$1 AND p.proname='lock_legacy'`, s.Schema).Scan(&body, &owner, &lang, &config, &sec, &nargs)
	if e != nil {
		return e
	}
	expected := "\nBEGIN\n    LOCK TABLE " + s.lockRelations() + " IN SHARE ROW EXCLUSIVE MODE;\n    LOCK TABLE " + scopeRelations(s) + " IN ROW EXCLUSIVE MODE;\nEND\n"
	if body != expected || owner != s.LockOwner || lang != "plpgsql" || config != "{search_path=pg_catalog}" || !sec || nargs != 0 {
		return errors.New("lock helper body/owner/config drift")
	}
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) a WHERE n.nspname=$1 AND p.proname='lock_legacy' AND (a.grantee NOT IN ((SELECT oid FROM pg_catalog.pg_roles WHERE rolname=$2),(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=$3)) OR a.is_grantable OR a.privilege_type <> 'EXECUTE')`, s.Schema, s.Loader, s.LockOwner).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("lock helper ACL drift")
	}
	if e = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace ns ON ns.oid=p.pronamespace WHERE p.prosecdef AND pg_catalog.has_function_privilege($1,p.oid,'EXECUTE') AND NOT (ns.nspname=$2 AND p.proname='lock_legacy' AND p.pronargs=0)`, s.Loader, s.Schema).Scan(&n); e != nil {
		return e
	}
	if n != 0 {
		return errors.New("alternate SECURITY DEFINER path")
	}
	var aclPresent bool
	if e = q.QueryRowContext(ctx, `SELECT p.proacl IS NOT NULL FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace ns ON ns.oid=p.pronamespace WHERE ns.nspname=$1 AND p.proname='lock_legacy'`, s.Schema).Scan(&aclPresent); e != nil {
		return e
	}
	if !aclPresent {
		return errors.New("default PUBLIC helper ACL")
	}
	return nil
}

// CatalogDigest binds physical relation identities, every column/constraint,
// index, trigger, owner, ACL, RLS policy and scope view/helper definition.
func CatalogDigest(ctx context.Context, q querier, scope string) (string, error) {
	rows, e := q.QueryContext(ctx, `SELECT line FROM (
 SELECT 'relation:'||c.oid||':'||(pg_catalog.to_jsonb(c)-ARRAY['relpages','reltuples','relallvisible','relfrozenxid','relminmxid'])::pg_catalog.text AS line FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('legacy',$1)
 UNION ALL SELECT 'column:'||pg_catalog.row_to_json(a)::pg_catalog.text FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('legacy',$1) AND a.attnum>0
 UNION ALL SELECT 'constraint:'||pg_catalog.row_to_json(k)::pg_catalog.text FROM pg_catalog.pg_constraint k JOIN pg_catalog.pg_namespace n ON n.oid=k.connamespace WHERE n.nspname IN ('legacy',$1)
 UNION ALL SELECT 'index:'||pg_catalog.row_to_json(i)::pg_catalog.text FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('legacy',$1)
 UNION ALL SELECT 'trigger:'||pg_catalog.row_to_json(t)::pg_catalog.text FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('legacy',$1)
 UNION ALL SELECT 'policy:'||pg_catalog.row_to_json(p)::pg_catalog.text FROM pg_catalog.pg_policy p JOIN pg_catalog.pg_class c ON c.oid=p.polrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('legacy',$1)
 UNION ALL SELECT 'rewrite:'||pg_catalog.row_to_json(r)::pg_catalog.text FROM pg_catalog.pg_rewrite r JOIN pg_catalog.pg_class c ON c.oid=r.ev_class JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('legacy',$1)
 UNION ALL SELECT 'function:'||pg_catalog.row_to_json(p)::pg_catalog.text FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN ('legacy',$1)
 UNION ALL SELECT 'namespace:'||pg_catalog.row_to_json(n)::pg_catalog.text FROM pg_catalog.pg_namespace n WHERE n.nspname IN ('legacy',$1)
 UNION ALL SELECT 'dependency:'||pg_catalog.row_to_json(d)::pg_catalog.text FROM pg_catalog.pg_depend d WHERE d.objid IN (SELECT c.oid FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('legacy',$1))
 ) entries ORDER BY line`, scope)
	if e != nil {
		return "", e
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			return "", e
		}
		h.Write([]byte(s + "\n"))
	}
	if e = rows.Err(); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func ScopeDigest(ctx context.Context, q querier, s Scope) (string, error) {
	// The stable source definition and effective role state supplement catalog ACLs.
	text, e := s.SQL()
	if e != nil {
		return "", e
	}
	h := sha256.New()
	h.Write([]byte(text))
	for _, n := range []string{s.Loader, s.ViewOwner, s.LockOwner} {
		var v string
		e = q.QueryRowContext(ctx, `SELECT pg_catalog.row_to_json(r)::pg_catalog.text FROM pg_catalog.pg_roles r WHERE rolname=$1`, n).Scan(&v)
		if e != nil {
			return "", e
		}
		h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
