-- notify domain: webhook signing/scope/delivery detail, FTS5 maintenance triggers.
ALTER TABLE webhooks ADD COLUMN secret TEXT;
ALTER TABLE webhooks ADD COLUMN user_id TEXT;
ALTER TABLE webhook_deliveries ADD COLUMN payload TEXT;
ALTER TABLE webhook_deliveries ADD COLUMN response_body TEXT;
ALTER TABLE webhook_deliveries ADD COLUMN next_attempt_at TEXT;
ALTER TABLE webhook_deliveries ADD COLUMN delivered_at TEXT;
ALTER TABLE webhook_deliveries ADD COLUMN duration_ms INTEGER;
CREATE INDEX webhook_deliveries_wh ON webhook_deliveries(webhook_id, created_at);
CREATE INDEX notif_card ON notifications(card_id, user_id);

-- Global search index: kind = card|comment|board|project. ref_id is the row id; board_id '' for projects.
-- search_map gives O(1) rowid lookup so updates never scan the FTS table.
CREATE TABLE search_map (kind TEXT NOT NULL, ref_id TEXT NOT NULL, rid INTEGER NOT NULL, PRIMARY KEY(kind,ref_id)) WITHOUT ROWID;

-- cards
CREATE TRIGGER notify_card_ai AFTER INSERT ON cards WHEN NEW.is_draft=0 AND NEW.is_template=0 BEGIN
  INSERT INTO search_idx(kind,ref_id,board_id,title,body) VALUES('card',NEW.id,NEW.board_id,NEW.name,COALESCE(NEW.description,''));
  INSERT OR REPLACE INTO search_map VALUES('card',NEW.id,last_insert_rowid());
END;
CREATE TRIGGER notify_card_au AFTER UPDATE OF name, description, board_id, is_draft, is_template ON cards BEGIN
  DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind='card' AND ref_id=OLD.id);
  DELETE FROM search_map WHERE kind='card' AND ref_id=OLD.id;
  INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'card',NEW.id,NEW.board_id,NEW.name,COALESCE(NEW.description,'') WHERE NEW.is_draft=0 AND NEW.is_template=0;
  INSERT OR REPLACE INTO search_map SELECT 'card',NEW.id,last_insert_rowid() WHERE NEW.is_draft=0 AND NEW.is_template=0;
  UPDATE search_idx SET board_id=NEW.board_id, title=NEW.name WHERE rowid IN (SELECT m.rid FROM search_map m JOIN comments c ON m.kind='comment' AND m.ref_id=c.id WHERE c.card_id=NEW.id);
END;
CREATE TRIGGER notify_card_ad AFTER DELETE ON cards BEGIN
  DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind='card' AND ref_id=OLD.id);
  DELETE FROM search_map WHERE kind='card' AND ref_id=OLD.id;
END;
-- comments
CREATE TRIGGER notify_comment_ai AFTER INSERT ON comments BEGIN
  INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'comment',NEW.id,c.board_id,c.name,NEW.text FROM cards c WHERE c.id=NEW.card_id;
  INSERT OR REPLACE INTO search_map SELECT 'comment',NEW.id,last_insert_rowid() WHERE EXISTS (SELECT 1 FROM cards WHERE id=NEW.card_id);
END;
CREATE TRIGGER notify_comment_au AFTER UPDATE OF text ON comments BEGIN
  DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind='comment' AND ref_id=OLD.id);
  DELETE FROM search_map WHERE kind='comment' AND ref_id=OLD.id;
  INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'comment',NEW.id,c.board_id,c.name,NEW.text FROM cards c WHERE c.id=NEW.card_id;
  INSERT OR REPLACE INTO search_map SELECT 'comment',NEW.id,last_insert_rowid() WHERE EXISTS (SELECT 1 FROM cards WHERE id=NEW.card_id);
END;
CREATE TRIGGER notify_comment_ad AFTER DELETE ON comments BEGIN
  DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind='comment' AND ref_id=OLD.id);
  DELETE FROM search_map WHERE kind='comment' AND ref_id=OLD.id;
END;
-- boards
CREATE TRIGGER notify_board_ai AFTER INSERT ON boards WHEN NEW.is_template=0 BEGIN
  INSERT INTO search_idx(kind,ref_id,board_id,title,body) VALUES('board',NEW.id,NEW.id,NEW.name,'');
  INSERT OR REPLACE INTO search_map VALUES('board',NEW.id,last_insert_rowid());
END;
CREATE TRIGGER notify_board_au AFTER UPDATE OF name, is_template ON boards BEGIN
  DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind='board' AND ref_id=OLD.id);
  DELETE FROM search_map WHERE kind='board' AND ref_id=OLD.id;
  INSERT INTO search_idx(kind,ref_id,board_id,title,body) SELECT 'board',NEW.id,NEW.id,NEW.name,'' WHERE NEW.is_template=0;
  INSERT OR REPLACE INTO search_map SELECT 'board',NEW.id,last_insert_rowid() WHERE NEW.is_template=0;
END;
CREATE TRIGGER notify_board_ad AFTER DELETE ON boards BEGIN
  DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind='board' AND ref_id=OLD.id);
  DELETE FROM search_map WHERE kind='board' AND ref_id=OLD.id;
END;
-- projects
CREATE TRIGGER notify_project_ai AFTER INSERT ON projects BEGIN
  INSERT INTO search_idx(kind,ref_id,board_id,title,body) VALUES('project',NEW.id,'',NEW.name,COALESCE(NEW.description,''));
  INSERT OR REPLACE INTO search_map VALUES('project',NEW.id,last_insert_rowid());
END;
CREATE TRIGGER notify_project_au AFTER UPDATE OF name, description ON projects BEGIN
  DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind='project' AND ref_id=OLD.id);
  DELETE FROM search_map WHERE kind='project' AND ref_id=OLD.id;
  INSERT INTO search_idx(kind,ref_id,board_id,title,body) VALUES('project',NEW.id,'',NEW.name,COALESCE(NEW.description,''));
  INSERT OR REPLACE INTO search_map VALUES('project',NEW.id,last_insert_rowid());
END;
CREATE TRIGGER notify_project_ad AFTER DELETE ON projects BEGIN
  DELETE FROM search_idx WHERE rowid IN (SELECT rid FROM search_map WHERE kind='project' AND ref_id=OLD.id);
  DELETE FROM search_map WHERE kind='project' AND ref_id=OLD.id;
END;
