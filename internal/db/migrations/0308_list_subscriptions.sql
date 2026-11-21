-- Watching a column (list.subscribe / list.unsubscribe): the watcher hears about cards created in it, moved into or out
-- of it, and new comments, close/re-open and due-date changes on the cards it holds (docs/DESIGN.md "Watching").
CREATE TABLE IF NOT EXISTS list_subscriptions (
  list_id TEXT NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY(list_id, user_id)
);
CREATE INDEX IF NOT EXISTS list_subscriptions_user ON list_subscriptions(user_id);
-- "What I watch" (subscription.list) reads board watches by user.
CREATE INDEX IF NOT EXISTS board_subscriptions_user ON board_subscriptions(user_id);
