-- Who may start a board from a published template (boards range 0300-0399).
-- private = only the publisher (and people who already have access to the board), group = also the members of
-- template_group_id, public = every signed-in user. Templates published before this migration were visible to all
-- project members only; they become public so existing behaviour is kept for them.
-- template_published_at anchors relative due dates: a card's due date is carried to new boards as the offset in
-- days between its due date and this moment.
ALTER TABLE boards ADD COLUMN template_visibility TEXT NOT NULL DEFAULT 'public';
ALTER TABLE boards ADD COLUMN template_group_id TEXT REFERENCES user_groups(id) ON DELETE SET NULL;
ALTER TABLE boards ADD COLUMN template_publisher_id TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE boards ADD COLUMN template_published_at TEXT;
UPDATE boards SET template_published_at=COALESCE(updated_at,created_at) WHERE is_template=1;
