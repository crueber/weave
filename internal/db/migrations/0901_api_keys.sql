-- 0901 (not 0112): the backfill reads users.is_agent (0800) and users.api_key_expires_at (0900), and migrations run in filename order.
-- Any number of API keys per person (auth domain). Until now a user had exactly one key, stored in the users columns
-- api_key_hash / api_key_prefix / api_key_created_at / api_key_expires_at, and generating a key replaced it. Keys now live in
-- their own table: one row per key (a person keeps one per computer or tool), each with its own name, expiry and last use.
-- Only the SHA-256 of a key is stored, never the key.
--
-- Backfill: every user that has a key today gets one row. SQL cannot call db.NewID, so the row id is the USER's id (unique:
-- there is at most one backfilled row per user); the key hash, prefix, created_at and expires_at are kept, so existing keys
-- (people's, agents', break-glass ones) keep working and keep their expiry.
--
-- The four old users columns stay where they are (SQLite cannot cheaply drop a NOT NULL/UNIQUE column) but are no longer read
-- or written by Weave (internal/db/apikey_scan_test.go enforces it). Their old values are left in place so that
-- rolling the binary back to the previous release still finds the keys that existed at upgrade time.
CREATE TABLE api_keys (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  key_hash     TEXT NOT NULL UNIQUE,
  prefix       TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  expires_at   TEXT,
  last_used_at TEXT
);
CREATE INDEX api_keys_user ON api_keys(user_id, created_at, id);

INSERT INTO api_keys(id,user_id,name,key_hash,prefix,created_at,expires_at)
  SELECT id, id, CASE WHEN is_agent=1 THEN 'Agent key' ELSE 'Existing key' END, api_key_hash, COALESCE(api_key_prefix,''),
         COALESCE(api_key_created_at, created_at), api_key_expires_at
  FROM users WHERE api_key_hash IS NOT NULL;
