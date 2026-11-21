-- Per-project override of the "Connect an agent" project prompt (plain text with {tokens}; NULL = the admin
-- default, else the built-in text). See internal/agentprompt.
ALTER TABLE projects ADD COLUMN agent_prompt TEXT;
