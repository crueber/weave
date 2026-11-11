-- Board colours (boards range 0300-0399).
-- boards.color: custom board colour, always a lowercase #rrggbb (presets are resolved to hex on write); NULL = none.
-- boards.card_color_mode: how labels colour cards on this board: 'pill' (default, label pills) or 'fill' (the card's
-- first label by position tints the whole tile; colours are never mixed).
ALTER TABLE boards ADD COLUMN color TEXT;
ALTER TABLE boards ADD COLUMN card_color_mode TEXT NOT NULL DEFAULT 'pill';
