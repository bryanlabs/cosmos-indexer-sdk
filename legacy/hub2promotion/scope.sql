-- NEW objects only. No ALTER of existing legacy relations, owners or triggers.
-- Rendered identifiers are validated; this migration is never run by the CLI.
CREATE ROLE {{view_owner}} NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
CREATE ROLE {{lock_owner}} NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
CREATE ROLE {{loader}} LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
CREATE SCHEMA {{scope}} AUTHORIZATION {{view_owner}};
REVOKE ALL ON SCHEMA {{scope}} FROM PUBLIC;
GRANT USAGE ON SCHEMA legacy TO {{view_owner}}, {{lock_owner}}, {{loader}};
GRANT SELECT ON {{all_tables}} TO {{loader}};
GRANT SELECT, INSERT ON {{insert_tables}} TO {{view_owner}};
-- UPDATE is held ONLY by the nonlogin helper owner for PG16 strong locks.
GRANT UPDATE ON {{all_tables}} TO {{lock_owner}};
{{views}}
GRANT USAGE ON SCHEMA {{scope}} TO {{loader}}, {{lock_owner}};
GRANT UPDATE ON {{view_tables}} TO {{lock_owner}};
CREATE FUNCTION {{scope}}.lock_legacy() RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $lock$
BEGIN
    LOCK TABLE {{all_tables}} IN SHARE ROW EXCLUSIVE MODE;
    LOCK TABLE {{view_tables}} IN ROW EXCLUSIVE MODE;
END
$lock$;
GRANT CREATE ON SCHEMA {{scope}} TO {{lock_owner}};
ALTER FUNCTION {{scope}}.lock_legacy() OWNER TO {{lock_owner}};
REVOKE CREATE ON SCHEMA {{scope}} FROM {{lock_owner}};
REVOKE ALL ON FUNCTION {{scope}}.lock_legacy() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION {{scope}}.lock_legacy() TO {{loader}};
