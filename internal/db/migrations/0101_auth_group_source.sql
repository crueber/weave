-- Groups created from the OIDC groups claim are marked so sync only manages those.
ALTER TABLE user_groups ADD COLUMN source TEXT NOT NULL DEFAULT 'local';
