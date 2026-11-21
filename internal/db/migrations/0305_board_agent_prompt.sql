-- Per-board override of the "Connect an agent" board prompt (plain text with {tokens}; NULL = the admin
-- default, else the built-in text). It travels with export/import and board templates.
ALTER TABLE boards ADD COLUMN agent_prompt TEXT;
