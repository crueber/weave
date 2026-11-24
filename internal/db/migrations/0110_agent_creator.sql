-- Owner-created AI agents: who created an agent account (users.is_agent=1). NULL for agents made before
-- this migration (admin-created) and for human accounts. The creator may rename, deactivate, rotate the key
-- of and delete the agent, and revoke its board access; granting access still needs Manage on the board.
ALTER TABLE users ADD COLUMN created_by TEXT;
CREATE INDEX users_created_by ON users(created_by) WHERE created_by IS NOT NULL;
