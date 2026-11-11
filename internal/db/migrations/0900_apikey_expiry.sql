-- Optional API-key expiry: NULL = the key never expires (the default).
ALTER TABLE users ADD COLUMN api_key_expires_at TEXT;
