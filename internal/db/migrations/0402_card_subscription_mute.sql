-- Muting a card: card.unsubscribe keeps (or creates) the row with muted=1. A muted card never notifies its user, even
-- when they watch its column or board; auto-watch (creating, commenting, being added) never re-adds it; card.subscribe
-- clears the mute. is_permanent now means "watched explicitly" (card.subscribe); auto-watch rows keep 0.
ALTER TABLE card_subscriptions ADD COLUMN muted INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS card_subscriptions_user ON card_subscriptions(user_id);
