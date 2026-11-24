-- cards domain: one card on several boards ("placements"). Phase 1: the table, the backfill and the home mirror; no
-- behaviour change (every card has exactly one placement, its home). See docs/DESIGN.md "Placements".
--
-- card_placements holds EVERY placement of a card, the home included (is_home=1). Per placement: the board, the column,
-- the position in it, the previous column (restore target) and when the card entered the column. Everything else is
-- per card, including the closed state (cards.is_closed is NOT mirrored; maintainer decision).
-- The columns cards.board_id/list_id/position/prev_list_id/list_changed_at stay as a read-only MIRROR of the home
-- placement, kept by the triggers below; cards_placement_guard rejects any other write to them, so a writer that was
-- not converted fails loudly instead of drifting. Writers update card_placements, never those columns on cards.
CREATE TABLE IF NOT EXISTS card_placements (
  card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
  board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
  list_id TEXT NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
  position REAL,
  prev_list_id TEXT,
  list_changed_at TEXT NOT NULL,
  is_home INTEGER NOT NULL DEFAULT 0,
  placed_by TEXT REFERENCES users(id) ON DELETE SET NULL,
  placed_at TEXT NOT NULL,
  PRIMARY KEY (card_id, board_id)                                            -- a card is never twice on one board
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS cp_home ON card_placements(card_id) WHERE is_home=1;  -- at most one home per card
CREATE INDEX IF NOT EXISTS cp_list ON card_placements(list_id, position);                -- column order, card.next, counts
CREATE INDEX IF NOT EXISTS cp_board ON card_placements(board_id);                        -- tiles, tree, stats, board delete

-- Backfill: one home placement per existing card, drafts and card templates included (they sit on a real column).
-- A plain INSERT on purpose: a row that cannot be written fails the upgrade (rolled back) instead of leaving a card
-- without a home, which the guard would then make immovable.
INSERT INTO card_placements(card_id,board_id,list_id,position,prev_list_id,list_changed_at,is_home,placed_by,placed_at)
  SELECT id,board_id,list_id,position,prev_list_id,list_changed_at,1,creator_user_id,created_at FROM cards
   WHERE id NOT IN (SELECT card_id FROM card_placements);

-- Every INSERT INTO cards (create, import, instantiate, duplicate, from-template, recurrence, drafts) gets its home.
CREATE TRIGGER IF NOT EXISTS cp_home_ai AFTER INSERT ON cards BEGIN
  INSERT INTO card_placements(card_id,board_id,list_id,position,prev_list_id,list_changed_at,is_home,placed_by,placed_at)
  VALUES(NEW.id,NEW.board_id,NEW.list_id,NEW.position,NEW.prev_list_id,NEW.list_changed_at,1,NEW.creator_user_id,NEW.created_at);
END;

-- The mirror, in two mutually exclusive triggers decided by the row's own OLD/NEW values:
-- cp_home_mirror: the home placement changed column/position/... on the SAME board. It never names cards.board_id,
--   because notify_card_au (0600) fires on "UPDATE OF board_id" even when the value is unchanged: a reorder,
--   list.sort or rebalance must not reindex the card in the search index.
-- cp_home_rehome: a placement became home (re-homing) or the home moved to another board (cross-board card.move,
--   draft publish onto another board). Sets the board and the list fields in ONE statement so the guard sees a
--   consistent row; notify_card_au then reindexes the card (and its comments) under the new board.
-- Both are restricted with UPDATE OF, so placed_by's ON DELETE SET NULL (a user delete) never rewrites cards.
CREATE TRIGGER IF NOT EXISTS cp_home_mirror AFTER UPDATE OF list_id, position, prev_list_id, list_changed_at ON card_placements
  WHEN NEW.is_home=1 AND OLD.is_home=1 AND NEW.board_id=OLD.board_id BEGIN
  UPDATE cards SET list_id=NEW.list_id, position=NEW.position, prev_list_id=NEW.prev_list_id, list_changed_at=NEW.list_changed_at
   WHERE id=NEW.card_id;
END;
CREATE TRIGGER IF NOT EXISTS cp_home_rehome AFTER UPDATE OF board_id, is_home ON card_placements
  WHEN NEW.is_home=1 AND (OLD.is_home=0 OR NEW.board_id<>OLD.board_id) BEGIN
  UPDATE cards SET board_id=NEW.board_id, list_id=NEW.list_id, position=NEW.position, prev_list_id=NEW.prev_list_id,
    list_changed_at=NEW.list_changed_at
   WHERE id=NEW.card_id;
END;

-- The guard: a write that names a mirrored column on cards must leave the row equal to the home placement. The
-- mirror triggers pass (the placement already holds the values); any other writer aborts the statement.
CREATE TRIGGER IF NOT EXISTS cards_placement_guard BEFORE UPDATE OF board_id, list_id, position, prev_list_id, list_changed_at ON cards
  WHEN NOT EXISTS (SELECT 1 FROM card_placements p WHERE p.card_id=NEW.id AND p.is_home=1 AND p.board_id=NEW.board_id
    AND p.list_id=NEW.list_id AND p.position IS NEW.position AND p.prev_list_id IS NEW.prev_list_id
    AND p.list_changed_at=NEW.list_changed_at) BEGIN
  SELECT RAISE(ABORT, 'cards.board_id/list_id/position/prev_list_id/list_changed_at mirror the home card_placements row: write the placement');
END;
