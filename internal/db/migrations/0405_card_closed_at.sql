-- cards domain: record when a card was closed (RFC3339 UTC timestamp) so the calendar view can
-- plot closed cards on their closed date.
ALTER TABLE cards ADD COLUMN closed_at TEXT;

-- Backfill existing closed cards from their last known modification or column-change timestamp.
UPDATE cards
   SET closed_at = COALESCE(updated_at, list_changed_at, created_at)
 WHERE is_closed = 1 AND closed_at IS NULL;

CREATE INDEX IF NOT EXISTS cards_closed_at ON cards(closed_at) WHERE closed_at IS NOT NULL;

-- Keep closed_at populated whenever is_closed transitions to 1 without an explicit closed_at,
-- and cleared whenever is_closed transitions back to 0.
CREATE TRIGGER IF NOT EXISTS cards_closed_at_ai AFTER INSERT ON cards
  WHEN NEW.is_closed = 1 AND NEW.closed_at IS NULL BEGIN
  UPDATE cards SET closed_at = COALESCE(NEW.updated_at, NEW.list_changed_at, NEW.created_at) WHERE id = NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS cards_closed_at_au_close AFTER UPDATE OF is_closed ON cards
  WHEN NEW.is_closed = 1 AND NEW.closed_at IS NULL BEGIN
  UPDATE cards SET closed_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS cards_closed_at_au_open AFTER UPDATE OF is_closed ON cards
  WHEN NEW.is_closed = 0 AND NEW.closed_at IS NOT NULL BEGIN
  UPDATE cards SET closed_at = NULL WHERE id = NEW.id;
END;
