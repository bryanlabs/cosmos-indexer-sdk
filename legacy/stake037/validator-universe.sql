BEGIN READ ONLY;
SET LOCAL statement_timeout = '120s';
SELECT json_build_object(
    'chain', 'cosmoshub-3',
    'through', 5200790,
    'heights_stored', (
        SELECT count(*) FROM legacy.blocks
        WHERE chain_id = 'cosmoshub-3' AND height BETWEEN 1 AND 5200790
    ),
    'validators', (
        SELECT json_agg(validator ORDER BY validator)
        FROM (
            SELECT validator_address AS validator
            FROM legacy.taxable_events
            WHERE chain_id = 'cosmoshub-3' AND height <= 5200790
              AND validator_address <> ''
            UNION
            SELECT operator FROM legacy.genesis_validators
            WHERE chain_id = 'cosmoshub-3'
            UNION
            SELECT validator FROM legacy.genesis_unbondings
            WHERE chain_id = 'cosmoshub-3'
        ) candidates
    )
);
COMMIT;
