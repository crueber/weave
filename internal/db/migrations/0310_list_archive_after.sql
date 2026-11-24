-- Column (list) auto-archive (boards range 0300-0399): "archive after N cards".
-- When set, the column keeps at most this many cards: whenever a landing (create, move, publish, duplicate, restore,
-- list.moveCards, import...) takes it over N, the cards that have been in the column longest (smallest list_changed_at)
-- go to the board's archive list (boards.Landed). NULL = off (0 is never stored; it clears the setting).
-- Additive and nullable: existing columns are unaffected.
ALTER TABLE lists ADD COLUMN archive_after INTEGER;
