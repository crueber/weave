-- cardextras: rows of repeatable custom-field groups, stopwatch owner, lookup indexes.
CREATE TABLE custom_field_rows (
  id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  group_id TEXT NOT NULL REFERENCES custom_field_groups(id) ON DELETE CASCADE, position REAL NOT NULL, created_at TEXT NOT NULL);
CREATE INDEX cfr_group ON custom_field_rows(card_id,group_id,position);
CREATE TABLE stopwatch_state (card_id TEXT PRIMARY KEY REFERENCES cards(id) ON DELETE CASCADE, user_id TEXT);
CREATE INDEX tl_card ON task_lists(card_id,position);
CREATE INDEX tasks_list ON tasks(task_list_id,position);
CREATE INDEX tasks_linked ON tasks(linked_card_id);
CREATE INDEX comments_card ON comments(card_id,created_at);
CREATE INDEX attachments_card ON attachments(card_id,created_at);
CREATE INDEX cfg_board ON custom_field_groups(board_id,position);
CREATE INDEX cfg_card ON custom_field_groups(card_id,position);
CREATE INDEX cf_group ON custom_fields(group_id,position);
CREATE INDEX cf_base ON custom_fields(base_group_id,position);
CREATE INDEX cfv_card ON custom_field_values(card_id);
CREATE INDEX time_card ON time_entries(card_id,started_at);
