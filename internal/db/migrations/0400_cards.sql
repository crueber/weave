-- cards domain: indexes for calendar/map/templates/share lookups and the recurrence sweep.
CREATE INDEX IF NOT EXISTS cards_due ON cards(board_id, due_date) WHERE due_date IS NOT NULL;
CREATE INDEX IF NOT EXISTS cards_recurring ON cards(due_date) WHERE recurrence IS NOT NULL;
CREATE INDEX IF NOT EXISTS cards_templates ON cards(template_project_id) WHERE is_template=1;
CREATE INDEX IF NOT EXISTS cards_drafts ON cards(board_id) WHERE is_draft=1;
CREATE INDEX IF NOT EXISTS card_editing_card ON card_editing(card_id);
