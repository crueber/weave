-- Board backgrounds: selectable flat patterns and per-board uploaded background images.
ALTER TABLE boards ADD COLUMN background_pattern TEXT;
ALTER TABLE boards ADD COLUMN background_image_id TEXT;

CREATE TABLE board_background_images (
  id TEXT PRIMARY KEY,
  board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  file TEXT NOT NULL,
  size INTEGER,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS board_background_images_board ON board_background_images(board_id);
