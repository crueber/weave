-- The legacy project-creator role is gone: every signed-in human with role admin or boardUser may create projects
-- (authz.CanCreateProject), so accounts that held it become plain board users. Nothing else changes: their project
-- manager rows, board memberships and everything they created stay as they are. The IdP group that used to grant
-- it is no longer read; its stale admin setting (oidc.ownerGroup) is removed so it does not linger in the config
-- table. An environment variable of that name is simply ignored.
UPDATE users SET role='boardUser' WHERE role='projectOwner';
DELETE FROM config WHERE key='oidc.ownerGroup';
