-- Review and apply separately before taxrepair. This CLI never migrates schema.
ALTER TABLE taxable_events
  ADD COLUMN IF NOT EXISTS validator_address text,
  ADD COLUMN IF NOT EXISTS reward_trigger text;
