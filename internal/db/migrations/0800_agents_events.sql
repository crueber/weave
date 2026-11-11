-- Agents, card claims and the durable event stream (replay for GET /events).

-- Service accounts for AI agents: no SSO login, authenticate with an API key only.
ALTER TABLE users ADD COLUMN is_agent INTEGER NOT NULL DEFAULT 0;

-- Claim lease on a card: an agent owns it until claimed_until unless it renews/releases.
ALTER TABLE cards ADD COLUMN claimed_by TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE cards ADD COLUMN claimed_until TEXT;
ALTER TABLE cards ADD COLUMN claim_note TEXT;
CREATE INDEX cards_claimed ON cards(claimed_until) WHERE claimed_by IS NOT NULL;

-- Event stream: every activity row written after this migration also carries a
-- monotonic sequence (the public event id), routing columns and the JSON envelope
-- (without id). Older rows keep seq NULL and are not replayable.
ALTER TABLE actions ADD COLUMN seq INTEGER;
ALTER TABLE actions ADD COLUMN etype TEXT;
ALTER TABLE actions ADD COLUMN list_id TEXT;
ALTER TABLE actions ADD COLUMN from_list_id TEXT;
ALTER TABLE actions ADD COLUMN to_list_id TEXT;
ALTER TABLE actions ADD COLUMN envelope TEXT;
CREATE UNIQUE INDEX actions_seq ON actions(seq) WHERE seq IS NOT NULL;
CREATE INDEX actions_seq_board ON actions(board_id, seq) WHERE seq IS NOT NULL;
CREATE INDEX actions_created ON actions(created_at);
