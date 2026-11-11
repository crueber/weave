-- auth domain additions (0100-0199)
ALTER TABLE users ADD COLUMN totp_locked_until TEXT;
ALTER TABLE users ADD COLUMN oidc_sub TEXT;
CREATE UNIQUE INDEX users_oidc_sub ON users(oidc_sub) WHERE oidc_sub IS NOT NULL;
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX trusted_devices_user ON trusted_devices(user_id);
