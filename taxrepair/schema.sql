-- Review and apply separately before taxrepair. This CLI never migrates schema.
ALTER TABLE taxable_events
  ADD COLUMN IF NOT EXISTS validator_address text,
  ADD COLUMN IF NOT EXISTS reward_trigger text,
  ADD COLUMN IF NOT EXISTS detail text;

-- Validator statements read by operator address; without this index every
-- statement scans the whole table. The partial index on legacy reward rows
-- lets the API find the methodology replay cursor (first reward row that the
-- current parser has not rewritten) with one lookup, and shrinks to nothing
-- when the replay is complete. Both were created CONCURRENTLY on mainnet on
-- 2026-10-15; AutoMigrate does not own them.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_tax_validator
  ON taxable_events (validator_address, category, timestamp)
  WHERE validator_address <> '';
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_tax_reward_legacy
  ON taxable_events (timestamp)
  WHERE category = 'reward' AND COALESCE(reward_trigger, '') = '';
