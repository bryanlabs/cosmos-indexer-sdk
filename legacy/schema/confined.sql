SELECT problem || ': ' || n FROM (
    SELECT 'role does not exist' AS problem,
        (SELECT 1 - count(*) FROM pg_roles WHERE rolname = '{{role}}') AS n
    UNION ALL SELECT 'role is a superuser or may create roles or databases, replicate or bypass row security',
        (SELECT count(*) FROM pg_roles WHERE rolname = '{{role}}'
         AND (rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls))
    UNION ALL SELECT 'role is a member of other roles',
        (SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member WHERE r.rolname = '{{role}}')
    UNION ALL SELECT 'role owns tables, views or sequences',
        (SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner WHERE r.rolname = '{{role}}')
    UNION ALL SELECT 'role owns schemas',
        (SELECT count(*) FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner WHERE r.rolname = '{{role}}')
    UNION ALL SELECT 'role owns databases',
        (SELECT count(*) FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE r.rolname = '{{role}}')
    UNION ALL SELECT 'role owns functions',
        (SELECT count(*) FROM pg_proc p JOIN pg_roles r ON r.oid = p.proowner WHERE r.rolname = '{{role}}')
    UNION ALL SELECT 'role may create schemas in this database',
        (SELECT count(*) WHERE has_database_privilege('{{role}}', current_database(), 'CREATE'))
    UNION ALL SELECT 'schemas the role may create objects in',
        (SELECT count(*) FROM pg_namespace n WHERE n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
         AND has_schema_privilege('{{role}}', n.oid, 'CREATE'))
    UNION ALL SELECT 'tables and views outside legacy the role may read or change, in whole or by column',
        (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname <> 'legacy' AND n.nspname <> 'pg_catalog' AND n.nspname <> 'information_schema'
         AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
         AND CASE WHEN c.relkind IN ('r', 'p', 'v', 'm', 'f')
             THEN has_table_privilege('{{role}}', c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
               OR has_any_column_privilege('{{role}}', c.oid, 'SELECT, INSERT, UPDATE, REFERENCES')
             ELSE false END)
    UNION ALL SELECT 'sequences outside legacy the role may use',
        (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname <> 'legacy' AND n.nspname <> 'pg_catalog' AND n.nspname <> 'information_schema'
         AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
         AND CASE WHEN c.relkind = 'S' THEN has_sequence_privilege('{{role}}', c.oid, 'USAGE, SELECT, UPDATE') ELSE false END)
    UNION ALL SELECT 'functions that run with their owner''s rights and the role may call',
        (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
         WHERE p.prosecdef AND n.nspname <> 'pg_catalog' AND n.nspname <> 'information_schema'
         AND has_function_privilege('{{role}}', p.oid, 'EXECUTE'))
    UNION ALL SELECT 'default privileges that would give the role new tables or sequences outside legacy',
        (SELECT count(*) FROM pg_default_acl d CROSS JOIN LATERAL aclexplode(d.defaclacl) a
         WHERE d.defaclobjtype IN ('r', 'S')
         AND d.defaclnamespace NOT IN (SELECT oid FROM pg_namespace WHERE nspname = 'legacy')
         AND (a.grantee = 0 OR a.grantee IN (SELECT oid FROM pg_roles WHERE rolname = '{{role}}')))
    UNION ALL SELECT 'legacy tables the role may truncate, alter through triggers or reference',
        (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'legacy' AND CASE WHEN c.relkind IN ('r', 'p')
             THEN has_table_privilege('{{role}}', c.oid, 'TRUNCATE, REFERENCES, TRIGGER')
               OR has_any_column_privilege('{{role}}', c.oid, 'REFERENCES')
             ELSE false END)
    UNION ALL SELECT 'written once tables the role may update or delete from, in whole or by column',
        (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'legacy' AND c.relname IN ({{written_once}})
         AND (has_table_privilege('{{role}}', c.oid, 'UPDATE, DELETE') OR has_any_column_privilege('{{role}}', c.oid, 'UPDATE')))
    UNION ALL SELECT 'legacy tables that are missing or the role may not add rows to',
        (SELECT {{written_once_count}} - count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'legacy' AND c.relname IN ({{written_once}})
         AND has_table_privilege('{{role}}', c.oid, 'SELECT') AND has_table_privilege('{{role}}', c.oid, 'INSERT'))
) checks WHERE n > 0 ORDER BY 1
