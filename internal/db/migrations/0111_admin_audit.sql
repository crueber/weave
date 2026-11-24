-- Audit log of admin-side actions that change who can reach what (auth domain, op audit.list, Admin console > Audit log):
-- agent.rotateKey, group.member.add and user.relink (plus the host command `weave admin relink`). One append-only row per
-- change, written in the same transaction as the change. Like project_recoveries (0203) nothing here references another
-- table with a foreign key: the trail must outlive the people and groups it names, so names and e-mail addresses are
-- snapshotted as text. actor_* are NULL for host commands (source = 'host'). details is a small JSON object of
-- non-secret facts (never a key, token, hash or secret; for a key rotation only the key's display prefix).
CREATE TABLE admin_audit (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  action TEXT NOT NULL,              -- agent.rotateKey | group.member.add | user.relink
  actor_id TEXT,
  actor_name TEXT,
  actor_email TEXT,
  target_kind TEXT NOT NULL,         -- agent | user
  target_id TEXT NOT NULL,
  target_name TEXT NOT NULL DEFAULT '',
  target_email TEXT NOT NULL DEFAULT '',
  details TEXT NOT NULL DEFAULT '{}',
  source TEXT NOT NULL               -- ui | api | host
);
CREATE INDEX admin_audit_created ON admin_audit(created_at, id);
CREATE INDEX admin_audit_action ON admin_audit(action, created_at, id);
