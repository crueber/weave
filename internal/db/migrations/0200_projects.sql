-- projects domain: solid-colour backgrounds and lookup indexes.
ALTER TABLE projects ADD COLUMN background_color TEXT;
CREATE INDEX IF NOT EXISTS background_images_project ON background_images(project_id);
CREATE INDEX IF NOT EXISTS base_cfg_project ON base_custom_field_groups(project_id);
CREATE INDEX IF NOT EXISTS custom_fields_base ON custom_fields(base_group_id);
