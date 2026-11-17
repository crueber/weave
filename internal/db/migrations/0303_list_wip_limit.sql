-- Column (list) soft WIP limit (boards range 0300-0399).
-- An advisory cap on the number of cards a column should hold. Nothing is blocked: the column header shows
-- "n/limit" and flags the column when it is over. NULL = no limit (0 is never stored; it clears the limit).
ALTER TABLE lists ADD COLUMN wip_limit INTEGER;
