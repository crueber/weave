-- Watching: "Watch cards I create" is now on by default for people.
-- Until now users.subscribe_own_cards defaulted to 0 and nothing ever invited anyone to change it, so a stored 0 is
-- indistinguishable from a deliberate choice; flip everyone to 1 once. Settings > Workspace still lets people turn it
-- off, and the auth domain creates new accounts with 1. Agents never auto-watch (the cards domain checks users.is_agent),
-- so the value does not matter for them (is_agent arrives in 0800, after this file on a fresh database).
UPDATE users SET subscribe_own_cards=1;
