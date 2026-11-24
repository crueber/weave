-- Audit trail of the admin recovery path for ownerless projects (projects.Reassign via `project.reassign`, and the
-- host-only `weave admin adopt`). One append-only row per recovery. Nothing here references another table with a
-- foreign key: the trail must outlive the project, the new manager and the acting admin, so their names and
-- e-mail addresses are snapshotted as text. actor_* are NULL for the host command (source = 'host').
-- previous is a JSON array of the managers the project had at that moment ({userId,name,email,state}).
CREATE TABLE project_recoveries (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  project_name TEXT NOT NULL,
  user_id TEXT NOT NULL,
  user_name TEXT NOT NULL,
  user_email TEXT NOT NULL,
  actor_id TEXT,
  actor_name TEXT,
  actor_email TEXT,
  source TEXT NOT NULL,              -- admin | host
  previous TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL
);
CREATE INDEX project_recoveries_created ON project_recoveries(created_at);
CREATE INDEX project_recoveries_project ON project_recoveries(project_id, created_at);
