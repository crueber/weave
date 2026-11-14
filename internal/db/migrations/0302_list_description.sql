-- Column (list) descriptions (boards range 0300-0399).
-- A short brief for the column: what a card that lands there is asking for. It is shown in the column
-- menu, in `board.tree`, and in the "Agent prompt" copied from the column, so an agent listening to the
-- column knows what to do without extra instructions. NULL = none.
ALTER TABLE lists ADD COLUMN description TEXT;
