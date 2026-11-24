-- Board owners manage board-scoped webhooks. created_by records who created a webhook; strict_dest=1 marks a
-- webhook created by a non-admin, which is delivered through the strict SSRF policy (no loopback, link-local,
-- metadata, multicast and, unless the admin allows it, no private-network destinations). The flag is set once at
-- creation and never relaxed, so an admin editing a user's webhook does not lift the policy.
ALTER TABLE webhooks ADD COLUMN created_by TEXT;
ALTER TABLE webhooks ADD COLUMN strict_dest INTEGER NOT NULL DEFAULT 0;
CREATE INDEX webhooks_board ON webhooks(board_id) WHERE board_id IS NOT NULL;
