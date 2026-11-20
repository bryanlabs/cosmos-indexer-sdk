-- Insert-only load of the withdraw-address plan written by
-- `taxrepair withdraw-address-plan`: for each reward the parser books to a
-- delegator whose withdraw address took the coins, the transfer row the
-- current parser appends after the message's other rows. The plan is read
-- from standard input. Nothing existing is updated or deleted.
--
-- A message that already holds a withdraw_address row is skipped, so a
-- rerun inserts nothing. Every other planned message must still hold
-- exactly the rows the plan was made against (count and fingerprint), and
-- each planned row is checked against the message's own stored data inside
-- Postgres: the delegator's booked reward in that denom covers it, and the
-- distribution module paid at least that much to the recipient in that
-- message's transfer events. Any failure aborts the whole transaction.
--
-- Run with -v apply=1 to commit; anything else rolls back (the dry run).
-- Each message is checked on its own, so a large plan can be split by
-- message id and loaded part by part, in any order, with the same result.
-- Every lookup is a per-message LATERAL on an indexed key with OFFSET 0.
\set ON_ERROR_STOP on
\if :{?apply}
\else
\set apply 0
\endif
BEGIN;
SET LOCAL statement_timeout = '900s';
SET LOCAL lock_timeout = '10s';
SET LOCAL TimeZone = 'UTC';
SET LOCAL jit = off;
SELECT pg_advisory_xact_lock(hashtext('taxable_events withdraw_address insert'));
\echo step plan
CREATE TEMP TABLE wa_plan (
  message_id bigint NOT NULL, sub_index bigint NOT NULL, category text, amount text, denom text, asset text,
  from_addr text, to_addr text, block_height bigint, "timestamp" timestamptz, tx_hash text,
  validator_address text, reward_trigger text, detail text, class text, trigger text,
  stored_rows bigint, stored_finger text, message_type_id bigint,
  PRIMARY KEY (message_id, sub_index)) ON COMMIT DROP;
\copy wa_plan FROM pstdin
ANALYZE wa_plan;
CREATE TEMP TABLE wa_msg ON COMMIT DROP AS
SELECT message_id, count(*) AS n, min(sub_index) AS lo, max(sub_index) AS hi,
       min(stored_rows) AS stored_rows, max(stored_rows) AS stored_rows2,
       min(stored_finger) AS finger, max(stored_finger) AS finger2,
       min(block_height) AS h, max(block_height) AS h2, min(tx_hash) AS hash, max(tx_hash) AS hash2,
       min("timestamp") AS ts, max("timestamp") AS ts2
  FROM wa_plan GROUP BY message_id;
ALTER TABLE wa_msg ADD PRIMARY KEY (message_id);
ANALYZE wa_msg;
SELECT 'plan', (SELECT count(*) FROM wa_msg) AS messages, (SELECT count(*) FROM wa_plan) AS rows;
\echo step plan guards
DO $$
DECLARE bad bigint;
BEGIN
  IF (SELECT count(*) FROM wa_plan) = 0 THEN RAISE EXCEPTION 'empty plan'; END IF;
  SELECT count(*) INTO bad FROM wa_plan
   WHERE category <> 'transfer' OR reward_trigger <> 'withdraw_address' OR amount !~ '^[1-9][0-9]*$'
      OR denom = '' OR coalesce(asset, '') <> '' OR coalesce(validator_address, '') <> ''
      OR from_addr !~ '^cosmos1[0-9a-z]{38,}$' OR to_addr !~ '^cosmos1[0-9a-z]{38,}$' OR from_addr = to_addr
      OR detail !~ '^\{"protocol":"distribution","reward_trigger":"[a-z_]+"\}$'
      OR class NOT IN ('exact', 'rewards_equal') OR message_id = 27907375;
  IF bad <> 0 THEN RAISE EXCEPTION '% plan rows fail the row shape guard', bad; END IF;
  SELECT count(*) INTO bad FROM wa_msg
   WHERE stored_rows <> stored_rows2 OR finger <> finger2 OR lo <> stored_rows OR hi <> stored_rows + n - 1
      OR h <> h2 OR hash <> hash2 OR ts <> ts2;
  IF bad <> 0 THEN RAISE EXCEPTION '% plan messages fail the numbering guard', bad; END IF;
END $$;
\echo step stored state
-- Lock the existing rows of every planned message for the transaction and
-- read them as they are now.
CREATE TEMP TABLE wa_now ON COMMIT DROP AS
SELECT w.message_id, s.n, s.max_sub, s.finger, s.moved, s.block_height, s.tx_hash, s."timestamp"
  FROM wa_msg w
  CROSS JOIN LATERAL (
    SELECT count(*) AS n, coalesce(max(te.sub_index), -1) AS max_sub,
           coalesce(md5(string_agg(concat_ws('|', te.id, te.sub_index, coalesce(te.category,''), coalesce(te.amount,''), coalesce(te.denom,''),
             coalesce(te.asset,''), coalesce(te.from_addr,''), coalesce(te.to_addr,''), coalesce(te.validator_address,''), coalesce(te.reward_trigger,''),
             coalesce(te.detail,''), coalesce(te.block_height,0), coalesce(extract(epoch FROM te.timestamp)::text,''), coalesce(te.tx_hash,'')), E'\n' ORDER BY te.sub_index)), '') AS finger,
           count(*) FILTER (WHERE te.reward_trigger = 'withdraw_address') AS moved,
           min(te.block_height) AS block_height, min(te.tx_hash) AS tx_hash, min(te."timestamp") AS "timestamp"
      FROM (SELECT * FROM taxable_events t WHERE t.message_id = w.message_id OFFSET 0 FOR UPDATE) te) s;
ALTER TABLE wa_now ADD PRIMARY KEY (message_id);
SELECT 'already moved, skipped', count(*) FROM wa_now WHERE moved <> 0;
DO $$
DECLARE bad bigint;
BEGIN
  SELECT count(*) INTO bad FROM wa_now n JOIN wa_msg w USING (message_id)
   WHERE n.moved = 0 AND (n.n <> w.stored_rows OR n.max_sub <> w.stored_rows - 1 OR n.finger <> w.finger
      OR n.block_height IS DISTINCT FROM w.h OR n.tx_hash IS DISTINCT FROM w.hash OR n."timestamp" IS DISTINCT FROM w.ts);
  IF bad <> 0 THEN RAISE EXCEPTION '% planned messages no longer hold the rows the plan was made against', bad; END IF;
END $$;
\echo step derive from stored data
-- The booked reward per delegator and denom in each message, from its
-- stored rows, and what the distribution module paid each recipient, from
-- its stored transfer events (recipient, sender, amount at consecutive
-- attribute indexes, which also covers merged legacy events).
CREATE TEMP TABLE wa_check ON COMMIT DROP AS
SELECT p.message_id, p.from_addr, p.to_addr, p.denom, sum(p.amount::numeric) AS planned
  FROM wa_plan p JOIN wa_now n USING (message_id) WHERE n.moved = 0
 GROUP BY 1, 2, 3, 4;
CREATE TEMP TABLE wa_booked ON COMMIT DROP AS
SELECT c.message_id, c.from_addr, c.denom, max(b.booked) AS booked, sum(c.planned) AS planned
  FROM wa_check c
  CROSS JOIN LATERAL (SELECT coalesce(sum(te.amount::numeric), 0) AS booked
                        FROM (SELECT * FROM taxable_events t WHERE t.message_id = c.message_id OFFSET 0) te
                       WHERE te.category = 'reward' AND te.to_addr = c.from_addr AND te.denom = c.denom) b
 GROUP BY 1, 2, 3;
CREATE TEMP TABLE wa_paid ON COMMIT DROP AS
SELECT c.message_id, c.to_addr, c.denom, c.planned, coalesce(pd.paid, 0) AS paid, pd.triples
  FROM (SELECT message_id, to_addr, denom, sum(planned) AS planned FROM wa_check GROUP BY 1, 2, 3) c
  CROSS JOIN LATERAL (
    SELECT sum(substring(coin FROM '^([0-9]+)')::numeric) FILTER (WHERE substring(coin FROM '^[0-9]+(.*)$') = c.denom) AS paid,
           count(DISTINCT r.id) AS triples
      FROM (SELECT e.id FROM message_events e WHERE e.message_id = c.message_id AND e.message_event_type_id = 5 OFFSET 0) e
      CROSS JOIN LATERAL (SELECT a.index, a.value FROM message_event_attributes a
                           WHERE a.message_event_id = e.id AND a.message_event_attribute_key_id = 25 AND a.value = c.to_addr OFFSET 0) rcpt
      CROSS JOIN LATERAL (SELECT a.id FROM message_event_attributes a
                           WHERE a.message_event_id = e.id AND a.index = rcpt.index + 1 AND a.message_event_attribute_key_id = 27
                             AND a.value = 'cosmos1jv65s3grqf6v6jl3dp4t6c9t9rk99cd88lyufl' OFFSET 0) r
      CROSS JOIN LATERAL (SELECT a.value FROM message_event_attributes a
                           WHERE a.message_event_id = e.id AND a.index = rcpt.index + 2 AND a.message_event_attribute_key_id = 17 OFFSET 0) amt
      CROSS JOIN LATERAL string_to_table(amt.value, ',') AS coin) pd;
DO $$
DECLARE bad bigint;
BEGIN
  SELECT count(*) INTO bad FROM wa_booked WHERE planned > booked;
  IF bad <> 0 THEN RAISE EXCEPTION '% delegator/denom moves exceed the stored booked reward', bad; END IF;
  SELECT count(*) INTO bad FROM wa_paid WHERE triples = 0 OR planned > paid;
  IF bad <> 0 THEN RAISE EXCEPTION '% recipient/denom moves exceed what the stored events show the distribution module paid', bad; END IF;
END $$;
\echo step insert
INSERT INTO taxable_events (message_id, sub_index, category, amount, denom, asset, from_addr, to_addr, block_height,
                            "timestamp", tx_hash, validator_address, reward_trigger, detail)
SELECT p.message_id, p.sub_index, p.category, p.amount, p.denom, coalesce(p.asset, ''), p.from_addr, p.to_addr, p.block_height,
       p."timestamp", p.tx_hash, coalesce(p.validator_address, ''), p.reward_trigger, p.detail
  FROM wa_plan p JOIN wa_now n USING (message_id)
 WHERE n.moved = 0
 ORDER BY p.message_id, p.sub_index;
\echo step post checks
DO $$
DECLARE bad bigint; want bigint;
BEGIN
  SELECT count(*) INTO want FROM wa_plan p JOIN wa_now n USING (message_id) WHERE n.moved = 0;
  SELECT count(*) INTO bad FROM wa_plan p JOIN wa_now n USING (message_id)
   CROSS JOIN LATERAL (SELECT * FROM taxable_events t WHERE t.message_id = p.message_id AND t.sub_index = p.sub_index OFFSET 0) te
   WHERE n.moved = 0 AND (te.category, te.amount, te.denom, te.asset, te.from_addr, te.to_addr, te.block_height, te."timestamp", te.tx_hash,
                          te.validator_address, te.reward_trigger, te.detail)
         IS NOT DISTINCT FROM (p.category, p.amount, p.denom, coalesce(p.asset, ''), p.from_addr, p.to_addr, p.block_height, p."timestamp", p.tx_hash,
                               coalesce(p.validator_address, ''), p.reward_trigger, p.detail);
  IF bad <> want THEN RAISE EXCEPTION 'post check: % of % planned rows read back equal', bad, want; END IF;
  SELECT count(*) INTO bad FROM wa_msg w JOIN wa_now n USING (message_id)
   CROSS JOIN LATERAL (
     SELECT count(*) AS k, coalesce(md5(string_agg(concat_ws('|', te.id, te.sub_index, coalesce(te.category,''), coalesce(te.amount,''), coalesce(te.denom,''),
             coalesce(te.asset,''), coalesce(te.from_addr,''), coalesce(te.to_addr,''), coalesce(te.validator_address,''), coalesce(te.reward_trigger,''),
             coalesce(te.detail,''), coalesce(te.block_height,0), coalesce(extract(epoch FROM te.timestamp)::text,''), coalesce(te.tx_hash,'')), E'\n' ORDER BY te.sub_index)), '') AS finger
       FROM (SELECT * FROM taxable_events t WHERE t.message_id = w.message_id OFFSET 0) te WHERE te.sub_index < w.stored_rows) s
   WHERE n.moved = 0 AND (s.k <> w.stored_rows OR s.finger <> w.finger);
  IF bad <> 0 THEN RAISE EXCEPTION 'post check: % messages changed their existing rows', bad; END IF;
END $$;
\echo step summary
SELECT 'inserted', p.trigger, p.class, count(DISTINCT p.message_id) AS messages, count(*) AS rows,
       round(sum(p.amount::numeric) FILTER (WHERE p.denom = 'uatom') / 1e6, 6) AS atom,
       count(*) FILTER (WHERE p.denom <> 'uatom') AS other_denom_rows
  FROM wa_plan p JOIN wa_now n USING (message_id) WHERE n.moved = 0
 GROUP BY ROLLUP (p.trigger, p.class) ORDER BY 2 NULLS LAST, 3 NULLS LAST;
\if :apply
COMMIT;
\echo committed
\else
ROLLBACK;
\echo dry run rolled back
\endif
