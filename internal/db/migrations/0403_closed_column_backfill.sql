-- "A card in a closed-type column is closed" (boards.Landed). Before this rule was enforced on every path, cards could
-- sit open in a closed column: moved there by list.moveCards, imported or instantiated from a template, or left there
-- when the column was switched to closed. Close them once. Drafts and card templates are not cards on the board.
-- updated_at is left alone (nothing about the card changed for a person), and no events or notifications are sent.
-- Cards that lost the closed state when they were archived or trashed earlier cannot be recovered.
UPDATE cards SET is_closed=1
 WHERE is_closed=0 AND is_draft=0 AND is_template=0
   AND list_id IN (SELECT id FROM lists WHERE type='closed');
