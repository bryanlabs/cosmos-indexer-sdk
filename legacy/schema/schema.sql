-- Cosmos Hub 1 to 3 history. Everything lives in the schema "legacy" and is
-- written by one role that holds no right on any other schema. Nothing here
-- adds a column to, reads or rewrites a row of an existing table.
--
-- Safe to run again: every statement is conditional. {{role}} is replaced
-- with the importer role name before the script runs.

CREATE SCHEMA IF NOT EXISTS legacy;
REVOKE ALL ON SCHEMA legacy FROM PUBLIC;

-- One row per chain of the ledger. The era number gives the order of the
-- chains; heights restart on each of the first three, so every ordering is
-- (era, height, phase, tx_index, msg_index, sub_index), never height alone.
CREATE TABLE IF NOT EXISTS legacy.eras (
    chain_id      text PRIMARY KEY,
    era           smallint NOT NULL UNIQUE CHECK (era BETWEEN 1 AND 4),
    first_height  bigint NOT NULL CHECK (first_height > 0),
    -- The height whose state was exported into the next chain's genesis file.
    -- Blocks after it exist on the old chain but were not carried over.
    export_height bigint,
    -- True when that export paid every pending reward and commission into
    -- balances with no transaction.
    export_paid_rewards boolean NOT NULL,
    -- The API reads an era only after its books close against the next
    -- genesis file.
    published     boolean NOT NULL DEFAULT false
);

INSERT INTO legacy.eras (chain_id, era, first_height, export_height, export_paid_rewards) VALUES
    ('cosmoshub-1', 1, 1, 500000, true),
    ('cosmoshub-2', 2, 1, 2902000, true),
    ('cosmoshub-3', 3, 1, 5200790, false),
    ('cosmoshub-4', 4, 5200791, NULL, false)
ON CONFLICT (chain_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS legacy.blocks (
    chain_id   text NOT NULL REFERENCES legacy.eras CHECK (chain_id <> 'cosmoshub-4'),
    height     bigint NOT NULL CHECK (height > 0),
    time       timestamptz NOT NULL,
    block_hash text NOT NULL CHECK (block_hash ~ '^[0-9A-F]{64}$'),
    proposer   text NOT NULL,
    tx_count   integer NOT NULL CHECK (tx_count >= 0),
    PRIMARY KEY (chain_id, height)
);

-- One appearance of a transaction in a block. The hash is evidence and is
-- not unique: the same bytes were included twice on cosmoshub-3.
CREATE TABLE IF NOT EXISTS legacy.tx_inclusions (
    chain_id      text NOT NULL,
    height        bigint NOT NULL,
    tx_index      integer NOT NULL CHECK (tx_index >= 0),
    hash          text NOT NULL CHECK (hash ~ '^[0-9A-F]{64}$'),
    code          bigint NOT NULL CHECK (code >= 0),
    codespace     text NOT NULL DEFAULT '',
    code_evidence text NOT NULL,
    gas_wanted    bigint NOT NULL,
    gas_used      bigint NOT NULL,
    fee           text NOT NULL,
    fee_payer     text NOT NULL,
    fee_charged   boolean NOT NULL,
    fee_evidence  text NOT NULL,
    memo          text NOT NULL,
    signers       text[] NOT NULL,
    raw           bytea NOT NULL,
    PRIMARY KEY (chain_id, height, tx_index),
    FOREIGN KEY (chain_id, height) REFERENCES legacy.blocks (chain_id, height)
);
CREATE INDEX IF NOT EXISTS tx_inclusions_hash ON legacy.tx_inclusions (hash);

-- Phase orders the rows of one height: 0 genesis (height 0), 1 begin block,
-- 2 transaction, 3 end block, 4 the payout made by the state export. Rows
-- outside phase 2 carry tx_index 0 and msg_index 0.
CREATE TABLE IF NOT EXISTS legacy.taxable_events (
    chain_id          text NOT NULL REFERENCES legacy.eras CHECK (chain_id <> 'cosmoshub-4'),
    height            bigint NOT NULL CHECK (height >= 0),
    phase             smallint NOT NULL CHECK (phase BETWEEN 0 AND 4),
    tx_index          integer NOT NULL CHECK (tx_index >= 0),
    msg_index         integer NOT NULL CHECK (msg_index >= 0),
    sub_index         integer NOT NULL CHECK (sub_index >= 0),
    category          text NOT NULL,
    amount            text NOT NULL CHECK (amount ~ '^[0-9]+$'),
    denom             text NOT NULL,
    asset             text NOT NULL DEFAULT '',
    from_addr         text NOT NULL DEFAULT '',
    to_addr           text NOT NULL DEFAULT '',
    validator_address text NOT NULL DEFAULT '',
    reward_trigger    text NOT NULL DEFAULT '',
    detail            text NOT NULL DEFAULT '',
    "timestamp"       timestamptz NOT NULL,
    tx_hash           text NOT NULL DEFAULT '',
    amount_source     text NOT NULL CHECK (amount_source IN
                          ('message', 'event', 'tag', 'state', 'export_payout', 'genesis', 'window_allocated')),
    PRIMARY KEY (chain_id, height, phase, tx_index, msg_index, sub_index),
    CHECK ((phase = 0) = (height = 0)),
    CHECK (phase = 2 OR (tx_index = 0 AND msg_index = 0))
);
CREATE INDEX IF NOT EXISTS taxable_events_from ON legacy.taxable_events (from_addr);
CREATE INDEX IF NOT EXISTS taxable_events_to ON legacy.taxable_events (to_addr);
CREATE INDEX IF NOT EXISTS taxable_events_time ON legacy.taxable_events ("timestamp");

-- Begin and end block events that say something happened to an account or
-- a validator: a slash, a completed unbonding, a proposal that ended, and
-- duplicate vote evidence carried by the block. Most carry no amount, so
-- they are evidence for rows worked out later, not rows themselves. The
-- per-validator allocation events of every block are not kept.
CREATE TABLE IF NOT EXISTS legacy.block_events (
    chain_id    text NOT NULL,
    height      bigint NOT NULL,
    phase       smallint NOT NULL CHECK (phase IN (1, 3)),
    event_index integer NOT NULL CHECK (event_index >= 0),
    type        text NOT NULL,
    -- An array of [key, value] pairs in the order the chain emitted them.
    attributes  jsonb NOT NULL,
    PRIMARY KEY (chain_id, height, phase, event_index),
    FOREIGN KEY (chain_id, height) REFERENCES legacy.blocks (chain_id, height)
);
CREATE INDEX IF NOT EXISTS block_events_type ON legacy.block_events (chain_id, type, height);

-- What was loaded from each genesis file, so a rerun can prove it loads the
-- same file and the row counts can be compared with the file.
CREATE TABLE IF NOT EXISTS legacy.genesis_files (
    chain_id           text PRIMARY KEY REFERENCES legacy.eras,
    sha256             text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    genesis_time       timestamptz NOT NULL,
    accounts           integer NOT NULL,
    delegations        integer NOT NULL,
    unbonding_entries  integer NOT NULL,
    validators         integer NOT NULL,
    withdraw_addresses integer NOT NULL,
    genesis_txs        integer NOT NULL,
    loaded_at          timestamptz NOT NULL DEFAULT now()
);

-- Amounts are uatom. liquid is the bank balance at the first block: the
-- coins the file lists, less any delegation a genesis transaction made.
-- delegated and unbonding are the sums of the two tables below.
CREATE TABLE IF NOT EXISTS legacy.genesis_accounts (
    chain_id         text NOT NULL REFERENCES legacy.genesis_files,
    address          text NOT NULL,
    file_position    integer NOT NULL,
    account_number   bigint NOT NULL,
    sequence         bigint NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('base', 'continuous_vesting', 'delayed_vesting', 'module')),
    module_name      text NOT NULL DEFAULT '',
    file_coins       numeric(40, 0) NOT NULL CHECK (file_coins >= 0),
    liquid           numeric(40, 0) NOT NULL CHECK (liquid >= 0),
    delegated        numeric(40, 0) NOT NULL CHECK (delegated >= 0),
    unbonding        numeric(40, 0) NOT NULL CHECK (unbonding >= 0),
    original_vesting numeric(40, 0) NOT NULL DEFAULT 0,
    delegated_free   numeric(40, 0) NOT NULL DEFAULT 0,
    delegated_vesting numeric(40, 0) NOT NULL DEFAULT 0,
    vesting_start    bigint NOT NULL DEFAULT 0,
    vesting_end      bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (chain_id, address),
    UNIQUE (chain_id, file_position)
);

-- shares is the figure the file states. tokens is what those shares were
-- worth at the validator's rate in the same file, rounded down, which is
-- what undelegating all of them would have returned.
CREATE TABLE IF NOT EXISTS legacy.genesis_delegations (
    chain_id   text NOT NULL REFERENCES legacy.genesis_files,
    delegator  text NOT NULL,
    validator  text NOT NULL,
    shares     numeric(60, 18) NOT NULL CHECK (shares > 0),
    tokens     numeric(40, 0) NOT NULL CHECK (tokens >= 0),
    from_genesis_tx boolean NOT NULL DEFAULT false,
    PRIMARY KEY (chain_id, delegator, validator)
);

CREATE TABLE IF NOT EXISTS legacy.genesis_unbondings (
    chain_id        text NOT NULL REFERENCES legacy.genesis_files,
    delegator       text NOT NULL,
    validator       text NOT NULL,
    entry           integer NOT NULL CHECK (entry >= 0),
    creation_height bigint NOT NULL,
    completion_time timestamptz NOT NULL,
    initial_balance numeric(40, 0) NOT NULL,
    balance         numeric(40, 0) NOT NULL,
    PRIMARY KEY (chain_id, delegator, validator, entry)
);

CREATE TABLE IF NOT EXISTS legacy.genesis_validators (
    chain_id         text NOT NULL REFERENCES legacy.genesis_files,
    operator         text NOT NULL,
    tokens           numeric(40, 0) NOT NULL,
    delegator_shares numeric(60, 18) NOT NULL,
    status           text NOT NULL,
    jailed           boolean NOT NULL,
    -- Unpaid at the moment of the export, as the file states them.
    outstanding_rewards    numeric(60, 18) NOT NULL DEFAULT 0,
    accumulated_commission numeric(60, 18) NOT NULL DEFAULT 0,
    PRIMARY KEY (chain_id, operator)
);

CREATE TABLE IF NOT EXISTS legacy.genesis_withdraw_addresses (
    chain_id         text NOT NULL REFERENCES legacy.genesis_files,
    delegator        text NOT NULL,
    withdraw_address text NOT NULL,
    PRIMARY KEY (chain_id, delegator)
);

CREATE TABLE IF NOT EXISTS legacy.reconciliation (
    chain_id   text NOT NULL REFERENCES legacy.eras,
    address    text NOT NULL,
    component  text NOT NULL,
    expected   numeric(60, 18) NOT NULL,
    computed   numeric(60, 18) NOT NULL,
    difference numeric(60, 18) NOT NULL,
    status     text NOT NULL,
    PRIMARY KEY (chain_id, address, component)
);

CREATE TABLE IF NOT EXISTS legacy.balance_days (
    chain_id text NOT NULL REFERENCES legacy.eras,
    address  text NOT NULL,
    day      date NOT NULL,
    height   bigint NOT NULL,
    holdings jsonb NOT NULL,
    PRIMARY KEY (chain_id, address, day)
);

CREATE TABLE IF NOT EXISTS legacy.import_ranges (
    chain_id       text NOT NULL REFERENCES legacy.eras,
    from_height    bigint NOT NULL CHECK (from_height > 0),
    to_height      bigint NOT NULL,
    status         text NOT NULL CHECK (status IN ('running', 'done', 'failed')),
    heights_stored bigint NOT NULL DEFAULT 0,
    heights_held   bigint NOT NULL DEFAULT 0,
    transactions   bigint NOT NULL DEFAULT 0,
    tax_rows       bigint NOT NULL DEFAULT 0,
    next_height    bigint NOT NULL,
    build          text NOT NULL DEFAULT '',
    started_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chain_id, from_height),
    CHECK (to_height >= from_height)
);

-- What the chain recorded is written once. No role, the owner included, can
-- change or remove a stored row while these triggers are enabled.
CREATE OR REPLACE FUNCTION legacy.refuse_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'legacy.% is written once', TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END $$ LANGUAGE plpgsql;

DO $$
DECLARE
    name text;
BEGIN
    FOREACH name IN ARRAY ARRAY['blocks', 'tx_inclusions', 'block_events', 'genesis_files', 'genesis_accounts',
        'genesis_delegations', 'genesis_unbondings', 'genesis_validators', 'genesis_withdraw_addresses']
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'written_once_rows'
                       AND tgrelid = format('legacy.%I', name)::regclass) THEN
            EXECUTE format('CREATE TRIGGER written_once_rows BEFORE UPDATE OR DELETE ON legacy.%I
                            FOR EACH ROW EXECUTE FUNCTION legacy.refuse_change()', name);
            EXECUTE format('CREATE TRIGGER written_once_table BEFORE TRUNCATE ON legacy.%I
                            FOR EACH STATEMENT EXECUTE FUNCTION legacy.refuse_change()', name);
        END IF;
    END LOOP;
END $$;

-- The importer role. It can log in only once a password is set, which is a
-- separate step. It belongs to no other role and is granted nothing outside
-- this schema.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{{role}}') THEN
        CREATE ROLE {{role}} LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
    END IF;
END $$;

DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO {{role}}', current_database());
END $$;

GRANT USAGE ON SCHEMA legacy TO {{role}};
GRANT SELECT ON ALL TABLES IN SCHEMA legacy TO {{role}};
GRANT INSERT ON legacy.blocks, legacy.tx_inclusions, legacy.block_events, legacy.taxable_events, legacy.genesis_files,
    legacy.genesis_accounts, legacy.genesis_delegations, legacy.genesis_unbondings,
    legacy.genesis_validators, legacy.genesis_withdraw_addresses TO {{role}};
GRANT INSERT, UPDATE, DELETE ON legacy.reconciliation, legacy.balance_days, legacy.import_ranges TO {{role}};

-- Nothing above is kept unless the role it leaves can add legacy rows and
-- reach nothing else. Any finding fails the migration.
DO $$
DECLARE
    finding text;
BEGIN
    FOR finding IN
        {{confined}}
    LOOP
        RAISE EXCEPTION 'role {{role}} is not confined: %', finding;
    END LOOP;
END $$;
