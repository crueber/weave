-- Performance indexes for hot authorization, board listing, session cleanup and event retention queries.
CREATE INDEX IF NOT EXISTS ugm_user ON user_group_members(user_id, group_id);
CREATE INDEX IF NOT EXISTS pm_user ON project_managers(user_id, project_id);
CREATE INDEX IF NOT EXISTS boards_project ON boards(project_id, position);
CREATE INDEX IF NOT EXISTS bm_user_only ON board_memberships(user_id, board_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS bm_group_only ON board_memberships(group_id, board_id) WHERE group_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at);
CREATE INDEX IF NOT EXISTS actions_envelope_created ON actions(created_at) WHERE envelope IS NOT NULL;
