-- One card on several boards (placements, 0404): one logical event writes one actions row per board it concerns.
-- The first row (the origin) is written as before; every other row of the same event names it in origin_id, so
-- readers that show an event once (a card's activity, the dashboard) group by COALESCE(origin_id, id). Rows written
-- before this migration keep origin_id NULL (each is its own origin).
ALTER TABLE actions ADD COLUMN origin_id TEXT;
CREATE INDEX actions_origin ON actions(origin_id) WHERE origin_id IS NOT NULL;
