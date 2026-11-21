-- Remove the board card-type settings (boards range 0300-0399).
-- Card types (project|story) were a Planka-heritage field that added nothing over labels; the board's
-- default card type and the "limit card types to the default" flag go with them. Neither column is used by
-- an index, trigger, view or constraint, so a plain DROP COLUMN keeps every other column, index and trigger.
ALTER TABLE boards DROP COLUMN default_card_type;
ALTER TABLE boards DROP COLUMN limit_card_types_to_default;
