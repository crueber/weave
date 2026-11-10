-- Weave core schema. Timestamps are RFC3339 UTC text. IDs are text (db.NewID()).
-- Positions are REAL with 65536 gaps (see docs/DESIGN.md).

CREATE TABLE users (
  id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE COLLATE NOCASE, username TEXT UNIQUE COLLATE NOCASE,
  password_hash TEXT, name TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'boardUser',            -- admin|projectOwner|boardUser|worker|guest
  avatar TEXT, phone TEXT, organization TEXT, language TEXT, subscribe_own_cards INTEGER NOT NULL DEFAULT 0,
  subscribe_to_card_when_commenting INTEGER NOT NULL DEFAULT 1, turn_off_recent_card_highlighting INTEGER NOT NULL DEFAULT 0,
  enable_favorites_by_default INTEGER NOT NULL DEFAULT 0, default_editor_mode TEXT NOT NULL DEFAULT 'wysiwyg',
  default_home_view TEXT NOT NULL DEFAULT 'groupedByProject', default_projects_order TEXT NOT NULL DEFAULT 'byDefault',
  theme TEXT, card_density TEXT, colour_blind_mode TEXT, auto_logout_minutes INTEGER,
  is_sso INTEGER NOT NULL DEFAULT 0, is_deactivated INTEGER NOT NULL DEFAULT 0, is_email_verified INTEGER NOT NULL DEFAULT 0,
  api_key_hash TEXT UNIQUE, api_key_prefix TEXT, api_key_created_at TEXT,
  totp_secret TEXT, totp_enabled INTEGER NOT NULL DEFAULT 0, totp_failed_attempts INTEGER NOT NULL DEFAULT 0,
  totp_recovery_hashes TEXT NOT NULL DEFAULT '[]', terms_accepted_at TEXT, terms_signature TEXT,
  presence TEXT NOT NULL DEFAULT 'offline', last_seen_at TEXT, dashboard TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL, updated_at TEXT, deleted_at TEXT
);
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, token_hash TEXT NOT NULL UNIQUE,
  remote_addr TEXT, user_agent TEXT, pending_totp INTEGER NOT NULL DEFAULT 0, pending_terms INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL, last_used_at TEXT
);
CREATE TABLE trusted_devices (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_hash TEXT NOT NULL, label TEXT, created_at TEXT NOT NULL);
CREATE TABLE user_groups (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);
CREATE TABLE user_group_members (group_id TEXT NOT NULL REFERENCES user_groups(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, PRIMARY KEY(group_id,user_id));

CREATE TABLE projects (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT, background_type TEXT, background_gradient TEXT,
  background_image_id TEXT, is_hidden INTEGER NOT NULL DEFAULT 0, owner_manager_id TEXT,
  use_project_labels INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT
);
CREATE TABLE project_managers (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, created_at TEXT NOT NULL, UNIQUE(project_id,user_id));
CREATE TABLE project_favorites (project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, PRIMARY KEY(project_id,user_id));
CREATE TABLE background_images (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  file TEXT NOT NULL, size INTEGER, created_at TEXT NOT NULL);

CREATE TABLE boards (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, position REAL NOT NULL, name TEXT NOT NULL,
  default_view TEXT NOT NULL DEFAULT 'kanban',        -- kanban|grid|list|calendar|map
  default_card_type TEXT NOT NULL DEFAULT 'project', limit_card_types_to_default INTEGER NOT NULL DEFAULT 0,
  always_display_card_creator INTEGER NOT NULL DEFAULT 0, expand_task_lists_by_default INTEGER NOT NULL DEFAULT 0,
  show_card_counter INTEGER NOT NULL DEFAULT 0, show_card_ages INTEGER NOT NULL DEFAULT 0,
  is_public INTEGER NOT NULL DEFAULT 0, public_token TEXT UNIQUE, is_template INTEGER NOT NULL DEFAULT 0,
  has_inbox INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT
);
CREATE TABLE board_memberships (id TEXT PRIMARY KEY, board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  user_id TEXT REFERENCES users(id) ON DELETE CASCADE, group_id TEXT REFERENCES user_groups(id) ON DELETE CASCADE,
  role TEXT NOT NULL DEFAULT 'editor',                -- editor|viewer
  can_comment INTEGER, created_at TEXT NOT NULL,
  CHECK ((user_id IS NULL) <> (group_id IS NULL)));
CREATE UNIQUE INDEX bm_user ON board_memberships(board_id,user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX bm_group ON board_memberships(board_id,group_id) WHERE group_id IS NOT NULL;
CREATE TABLE board_subscriptions (board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, PRIMARY KEY(board_id,user_id));
CREATE TABLE saved_filters (id TEXT PRIMARY KEY, board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, name TEXT NOT NULL, query TEXT NOT NULL, created_at TEXT NOT NULL);

CREATE TABLE label_groups (id TEXT PRIMARY KEY, board_id TEXT REFERENCES boards(id) ON DELETE CASCADE,
  project_id TEXT REFERENCES projects(id) ON DELETE CASCADE, name TEXT NOT NULL, single_select INTEGER NOT NULL DEFAULT 0, position REAL NOT NULL);
CREATE TABLE labels (id TEXT PRIMARY KEY, board_id TEXT REFERENCES boards(id) ON DELETE CASCADE,
  project_id TEXT REFERENCES projects(id) ON DELETE CASCADE, group_id TEXT REFERENCES label_groups(id) ON DELETE SET NULL,
  name TEXT, color TEXT NOT NULL, position REAL NOT NULL, created_at TEXT NOT NULL);

CREATE TABLE lists (id TEXT PRIMARY KEY, board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  type TEXT NOT NULL DEFAULT 'active',                -- active|closed|archive|trash
  position REAL, name TEXT, color TEXT, created_at TEXT NOT NULL, updated_at TEXT);
CREATE TABLE cards (
  id TEXT PRIMARY KEY, board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE, list_id TEXT NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
  creator_user_id TEXT REFERENCES users(id) ON DELETE SET NULL, prev_list_id TEXT, cover_attachment_id TEXT,
  type TEXT NOT NULL DEFAULT 'project', position REAL, name TEXT NOT NULL, description TEXT, due_date TEXT, is_due_completed INTEGER,
  stopwatch_started_at TEXT, stopwatch_total_seconds INTEGER NOT NULL DEFAULT 0,
  comments_total INTEGER NOT NULL DEFAULT 0, is_closed INTEGER NOT NULL DEFAULT 0, is_draft INTEGER NOT NULL DEFAULT 0, is_template INTEGER NOT NULL DEFAULT 0,
  lat REAL, lng REAL, location_name TEXT, recurrence TEXT,   -- recurrence: JSON {freq,interval,...}
  share_token TEXT UNIQUE, list_changed_at TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT, template_project_id TEXT
);
CREATE INDEX cards_list ON cards(list_id,position); CREATE INDEX cards_board ON cards(board_id);
CREATE TABLE card_memberships (card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, PRIMARY KEY(card_id,user_id));
CREATE TABLE card_labels (card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE, label_id TEXT NOT NULL REFERENCES labels(id) ON DELETE CASCADE, PRIMARY KEY(card_id,label_id));
CREATE TABLE card_subscriptions (card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, is_permanent INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(card_id,user_id));
CREATE TABLE time_entries (id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE, user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
  started_at TEXT NOT NULL, seconds INTEGER NOT NULL, note TEXT);
CREATE TABLE card_editing (card_id TEXT NOT NULL, user_id TEXT NOT NULL, field TEXT NOT NULL, touched_at TEXT NOT NULL, PRIMARY KEY(card_id,user_id,field));

CREATE TABLE task_lists (id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE, position REAL NOT NULL, name TEXT NOT NULL,
  show_on_front_of_card INTEGER NOT NULL DEFAULT 0, hide_completed_tasks INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE TABLE tasks (id TEXT PRIMARY KEY, task_list_id TEXT NOT NULL REFERENCES task_lists(id) ON DELETE CASCADE, linked_card_id TEXT REFERENCES cards(id) ON DELETE SET NULL,
  assignee_user_id TEXT REFERENCES users(id) ON DELETE SET NULL, position REAL NOT NULL, name TEXT NOT NULL, is_completed INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE TABLE attachments (id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE, creator_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
  type TEXT NOT NULL,                                 -- file|link
  name TEXT NOT NULL, file TEXT, url TEXT, mime TEXT, size INTEGER, is_image INTEGER NOT NULL DEFAULT 0, width INTEGER, height INTEGER, created_at TEXT NOT NULL);
CREATE TABLE comments (id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE, user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
  text TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT);

CREATE TABLE base_custom_field_groups (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, name TEXT, created_at TEXT NOT NULL);
CREATE TABLE custom_field_groups (id TEXT PRIMARY KEY, board_id TEXT REFERENCES boards(id) ON DELETE CASCADE, card_id TEXT REFERENCES cards(id) ON DELETE CASCADE,
  base_group_id TEXT REFERENCES base_custom_field_groups(id) ON DELETE SET NULL, name TEXT, repeatable INTEGER NOT NULL DEFAULT 0, position REAL NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE custom_fields (id TEXT PRIMARY KEY, group_id TEXT REFERENCES custom_field_groups(id) ON DELETE CASCADE,
  base_group_id TEXT REFERENCES base_custom_field_groups(id) ON DELETE CASCADE, name TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'text',                  -- text|number|date|checkbox|select|url
  options TEXT, position REAL NOT NULL, show_on_front_of_card INTEGER NOT NULL DEFAULT 0);
CREATE TABLE custom_field_values (id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  group_id TEXT NOT NULL REFERENCES custom_field_groups(id) ON DELETE CASCADE, field_id TEXT NOT NULL REFERENCES custom_fields(id) ON DELETE CASCADE,
  row_id TEXT NOT NULL DEFAULT '', content TEXT, UNIQUE(card_id,group_id,field_id,row_id));

CREATE TABLE actions (id TEXT PRIMARY KEY, board_id TEXT NOT NULL, card_id TEXT, user_id TEXT, type TEXT NOT NULL, data TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL);
CREATE INDEX actions_board ON actions(board_id,created_at); CREATE INDEX actions_card ON actions(card_id,created_at);
CREATE TABLE notifications (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, creator_user_id TEXT, board_id TEXT, card_id TEXT, comment_id TEXT,
  action_id TEXT, type TEXT NOT NULL, data TEXT NOT NULL DEFAULT '{}', is_read INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE INDEX notif_user ON notifications(user_id,is_read);
CREATE TABLE notification_services (id TEXT PRIMARY KEY, user_id TEXT REFERENCES users(id) ON DELETE CASCADE, board_id TEXT REFERENCES boards(id) ON DELETE CASCADE,
  url TEXT NOT NULL, format TEXT NOT NULL DEFAULT 'text', created_at TEXT NOT NULL);
CREATE TABLE webhooks (id TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL, access_token TEXT, event_types TEXT, excluded_event_types TEXT,
  project_id TEXT, board_id TEXT, created_at TEXT NOT NULL, updated_at TEXT);
CREATE TABLE webhook_deliveries (id TEXT PRIMARY KEY, webhook_id TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE, event TEXT NOT NULL, status INTEGER, error TEXT, attempts INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);

CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT NOT NULL);   -- smtp.*, oidc.*, branding.*, registration, terms...
CREATE TABLE storage_usage (id INTEGER PRIMARY KEY CHECK (id=1), bytes INTEGER NOT NULL DEFAULT 0);
CREATE TABLE board_imports (id TEXT PRIMARY KEY, user_id TEXT, source TEXT, status TEXT, created_at TEXT NOT NULL);

CREATE VIRTUAL TABLE search_idx USING fts5(kind UNINDEXED, ref_id UNINDEXED, board_id UNINDEXED, title, body, tokenize='porter unicode61');
