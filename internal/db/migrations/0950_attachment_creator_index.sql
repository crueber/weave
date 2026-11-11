-- Per-user upload quota (WEAVE_USER_STORAGE_MB) sums attachment sizes by uploader.
CREATE INDEX IF NOT EXISTS attachments_creator ON attachments(creator_user_id);
