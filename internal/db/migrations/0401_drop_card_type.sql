-- Remove the card type (cards range 0400-0499).
-- cards.type (project|story) was a Planka-heritage field that added nothing over labels. It is not used by an
-- index, trigger (the search triggers in 0600 watch name/description/board_id/is_draft/is_template), view or
-- constraint, so a plain DROP COLUMN keeps the data, indexes, triggers and foreign keys of the table intact.
ALTER TABLE cards DROP COLUMN type;
