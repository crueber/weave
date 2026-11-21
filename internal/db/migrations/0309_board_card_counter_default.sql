-- Boards: the "card counter" setting (show the number of cards in each column header) was stored but never read,
-- so every board showed its counts regardless. The view now honours it. Counts have always been visible and 0 was
-- only the unused column default, so a stored 0 cannot be told from a choice: turn it on everywhere to keep every
-- board looking as it does today (people can switch it off in board settings, and it now takes effect).
-- New boards are created with the counter on (boards.go) and imports/templates default to on when the file is silent.
UPDATE boards SET show_card_counter=1;
