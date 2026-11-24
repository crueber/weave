-- Who may create boards in a project (authz.CanCreateBoard): 'owners' = project managers only (the default and the
-- behaviour before this column existed), 'members' = also any project member (anyone with a membership on a board of
-- the project) who may create projects. A creator who is not a project manager becomes the new board's owner
-- (board_memberships.role = 'owner', which grants Manage on that board only).
ALTER TABLE projects ADD COLUMN board_creation TEXT NOT NULL DEFAULT 'owners';
