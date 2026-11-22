-- +goose Up
-- Each search index gains a terms column filled by the Go search_terms()
-- function, so ё/е spellings and Russian word forms match each other.
DROP TRIGGER message_search_fts_ai;
DROP TRIGGER message_search_fts_bd;
DROP TRIGGER message_search_fts_bu;
DROP TRIGGER message_search_fts_au;
DROP TABLE message_search_fts;
DROP VIEW message_search_content;

CREATE VIEW message_search_content AS
SELECT d.id, d.thread_id, d.seq, m.content, search_terms(m.content) AS terms
FROM message_search_docs d
JOIN messages m ON m.thread_id = d.thread_id AND m.seq = d.seq;

CREATE VIRTUAL TABLE message_search_fts USING fts5(
  content,
  terms,
  content='message_search_content',
  content_rowid='id',
  tokenize='unicode61',
  prefix='2 3 4'
);
INSERT INTO message_search_fts(message_search_fts) VALUES ('rebuild');

-- +goose StatementBegin
CREATE TRIGGER message_search_fts_ai AFTER INSERT ON messages
WHEN new.role IN ('user', 'assistant') AND trim(new.content) <> ''
BEGIN
  INSERT INTO message_search_docs(thread_id, seq) VALUES (new.thread_id, new.seq);
  INSERT INTO message_search_fts(rowid, content, terms)
  SELECT id, content, terms FROM message_search_content
  WHERE thread_id = new.thread_id AND seq = new.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_search_fts_bd BEFORE DELETE ON messages
BEGIN
  INSERT INTO message_search_fts(message_search_fts, rowid, content, terms)
  SELECT 'delete', id, content, terms FROM message_search_content
  WHERE thread_id = old.thread_id AND seq = old.seq;
  DELETE FROM message_search_docs WHERE thread_id = old.thread_id AND seq = old.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_search_fts_bu BEFORE UPDATE ON messages
BEGIN
  INSERT INTO message_search_fts(message_search_fts, rowid, content, terms)
  SELECT 'delete', id, content, terms FROM message_search_content
  WHERE thread_id = old.thread_id AND seq = old.seq;
  DELETE FROM message_search_docs WHERE thread_id = old.thread_id AND seq = old.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_search_fts_au AFTER UPDATE ON messages
BEGIN
  INSERT INTO message_search_docs(thread_id, seq)
  SELECT new.thread_id, new.seq
  WHERE new.role IN ('user', 'assistant') AND trim(new.content) <> '';
  INSERT INTO message_search_fts(rowid, content, terms)
  SELECT id, content, terms FROM message_search_content
  WHERE thread_id = new.thread_id AND seq = new.seq;
END;
-- +goose StatementEnd

DROP TRIGGER event_search_fts_ai;
DROP TRIGGER event_search_fts_bd;
DROP TRIGGER event_search_fts_bu;
DROP TRIGGER event_search_fts_au;
DROP TABLE event_search_fts;
DROP VIEW event_search_content;

CREATE VIEW event_search_content AS
SELECT d.id, d.thread_id, d.seq, e.content, search_terms(e.content) AS terms
FROM event_search_docs d
JOIN session_events e ON e.thread_id = d.thread_id AND e.seq = d.seq;

CREATE VIRTUAL TABLE event_search_fts USING fts5(
  content,
  terms,
  content='event_search_content',
  content_rowid='id',
  tokenize='unicode61',
  prefix='2 3 4'
);
INSERT INTO event_search_fts(event_search_fts) VALUES ('rebuild');

-- +goose StatementBegin
CREATE TRIGGER event_search_fts_ai AFTER INSERT ON session_events
WHEN new.type = 'acp_message' AND trim(new.content) <> ''
  AND json_valid(new.acp) AND json_extract(new.acp, '$.id') = new.thread_id
BEGIN
  INSERT INTO event_search_docs(thread_id, seq) VALUES (new.thread_id, new.seq);
  INSERT INTO event_search_fts(rowid, content, terms)
  SELECT id, content, terms FROM event_search_content
  WHERE thread_id = new.thread_id AND seq = new.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER event_search_fts_bd BEFORE DELETE ON session_events
BEGIN
  INSERT INTO event_search_fts(event_search_fts, rowid, content, terms)
  SELECT 'delete', id, content, terms FROM event_search_content
  WHERE thread_id = old.thread_id AND seq = old.seq;
  DELETE FROM event_search_docs WHERE thread_id = old.thread_id AND seq = old.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER event_search_fts_bu BEFORE UPDATE ON session_events
BEGIN
  INSERT INTO event_search_fts(event_search_fts, rowid, content, terms)
  SELECT 'delete', id, content, terms FROM event_search_content
  WHERE thread_id = old.thread_id AND seq = old.seq;
  DELETE FROM event_search_docs WHERE thread_id = old.thread_id AND seq = old.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER event_search_fts_au AFTER UPDATE ON session_events
WHEN new.type = 'acp_message' AND trim(new.content) <> ''
  AND json_valid(new.acp) AND json_extract(new.acp, '$.id') = new.thread_id
BEGIN
  INSERT INTO event_search_docs(thread_id, seq) VALUES (new.thread_id, new.seq);
  INSERT INTO event_search_fts(rowid, content, terms)
  SELECT id, content, terms FROM event_search_content
  WHERE thread_id = new.thread_id AND seq = new.seq;
END;
-- +goose StatementEnd

DROP TRIGGER thread_search_fts_ai;
DROP TRIGGER thread_search_fts_bd;
DROP TRIGGER thread_search_fts_bu;
DROP TRIGGER thread_search_fts_au;
DROP TABLE thread_search_fts;
DROP VIEW thread_search_content;

CREATE VIEW thread_search_content AS
SELECT
  d.id,
  d.thread_id,
  coalesce(t.title, '') AS title,
  t.slug,
  coalesce(t.project_path, '') AS project_path,
  search_terms(coalesce(t.title, '')) AS terms
FROM thread_search_docs d
JOIN threads t ON t.id = d.thread_id;

CREATE VIRTUAL TABLE thread_search_fts USING fts5(
  title,
  slug,
  project_path,
  terms,
  content='thread_search_content',
  content_rowid='id',
  tokenize='unicode61',
  prefix='2 3 4'
);
INSERT INTO thread_search_fts(thread_search_fts) VALUES ('rebuild');

-- +goose StatementBegin
CREATE TRIGGER thread_search_fts_ai AFTER INSERT ON threads
BEGIN
  INSERT INTO thread_search_docs(thread_id) VALUES (new.id);
  INSERT INTO thread_search_fts(rowid, title, slug, project_path, terms)
  SELECT id, title, slug, project_path, terms FROM thread_search_content
  WHERE thread_id = new.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER thread_search_fts_bd BEFORE DELETE ON threads
BEGIN
  INSERT INTO thread_search_fts(thread_search_fts, rowid, title, slug, project_path, terms)
  SELECT 'delete', id, title, slug, project_path, terms FROM thread_search_content
  WHERE thread_id = old.id;
  DELETE FROM thread_search_docs WHERE thread_id = old.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER thread_search_fts_bu BEFORE UPDATE OF title, slug, project_path ON threads
BEGIN
  INSERT INTO thread_search_fts(thread_search_fts, rowid, title, slug, project_path, terms)
  SELECT 'delete', id, title, slug, project_path, terms FROM thread_search_content
  WHERE thread_id = old.id;
  DELETE FROM thread_search_docs WHERE thread_id = old.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER thread_search_fts_au AFTER UPDATE OF title, slug, project_path ON threads
BEGIN
  INSERT INTO thread_search_docs(thread_id) VALUES (new.id);
  INSERT INTO thread_search_fts(rowid, title, slug, project_path, terms)
  SELECT id, title, slug, project_path, terms FROM thread_search_content
  WHERE thread_id = new.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER message_search_fts_ai;
DROP TRIGGER message_search_fts_bd;
DROP TRIGGER message_search_fts_bu;
DROP TRIGGER message_search_fts_au;
DROP TABLE message_search_fts;
DROP VIEW message_search_content;

CREATE VIEW message_search_content AS
SELECT d.id, d.thread_id, d.seq, m.content
FROM message_search_docs d
JOIN messages m ON m.thread_id = d.thread_id AND m.seq = d.seq;

CREATE VIRTUAL TABLE message_search_fts USING fts5(
  content,
  content='message_search_content',
  content_rowid='id',
  tokenize='unicode61',
  prefix='2 3 4'
);
INSERT INTO message_search_fts(message_search_fts) VALUES ('rebuild');

-- +goose StatementBegin
CREATE TRIGGER message_search_fts_ai AFTER INSERT ON messages
WHEN new.role IN ('user', 'assistant') AND trim(new.content) <> ''
BEGIN
  INSERT INTO message_search_docs(thread_id, seq) VALUES (new.thread_id, new.seq);
  INSERT INTO message_search_fts(rowid, content)
  SELECT id, content FROM message_search_content
  WHERE thread_id = new.thread_id AND seq = new.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_search_fts_bd BEFORE DELETE ON messages
BEGIN
  INSERT INTO message_search_fts(message_search_fts, rowid, content)
  SELECT 'delete', id, content FROM message_search_content
  WHERE thread_id = old.thread_id AND seq = old.seq;
  DELETE FROM message_search_docs WHERE thread_id = old.thread_id AND seq = old.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_search_fts_bu BEFORE UPDATE ON messages
BEGIN
  INSERT INTO message_search_fts(message_search_fts, rowid, content)
  SELECT 'delete', id, content FROM message_search_content
  WHERE thread_id = old.thread_id AND seq = old.seq;
  DELETE FROM message_search_docs WHERE thread_id = old.thread_id AND seq = old.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER message_search_fts_au AFTER UPDATE ON messages
BEGIN
  INSERT INTO message_search_docs(thread_id, seq)
  SELECT new.thread_id, new.seq
  WHERE new.role IN ('user', 'assistant') AND trim(new.content) <> '';
  INSERT INTO message_search_fts(rowid, content)
  SELECT id, content FROM message_search_content
  WHERE thread_id = new.thread_id AND seq = new.seq;
END;
-- +goose StatementEnd

DROP TRIGGER event_search_fts_ai;
DROP TRIGGER event_search_fts_bd;
DROP TRIGGER event_search_fts_bu;
DROP TRIGGER event_search_fts_au;
DROP TABLE event_search_fts;
DROP VIEW event_search_content;

CREATE VIEW event_search_content AS
SELECT d.id, d.thread_id, d.seq, e.content
FROM event_search_docs d
JOIN session_events e ON e.thread_id = d.thread_id AND e.seq = d.seq;

CREATE VIRTUAL TABLE event_search_fts USING fts5(
  content,
  content='event_search_content',
  content_rowid='id',
  tokenize='unicode61',
  prefix='2 3 4'
);
INSERT INTO event_search_fts(event_search_fts) VALUES ('rebuild');

-- +goose StatementBegin
CREATE TRIGGER event_search_fts_ai AFTER INSERT ON session_events
WHEN new.type = 'acp_message' AND trim(new.content) <> ''
  AND json_valid(new.acp) AND json_extract(new.acp, '$.id') = new.thread_id
BEGIN
  INSERT INTO event_search_docs(thread_id, seq) VALUES (new.thread_id, new.seq);
  INSERT INTO event_search_fts(rowid, content)
  SELECT id, content FROM event_search_content
  WHERE thread_id = new.thread_id AND seq = new.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER event_search_fts_bd BEFORE DELETE ON session_events
BEGIN
  INSERT INTO event_search_fts(event_search_fts, rowid, content)
  SELECT 'delete', id, content FROM event_search_content
  WHERE thread_id = old.thread_id AND seq = old.seq;
  DELETE FROM event_search_docs WHERE thread_id = old.thread_id AND seq = old.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER event_search_fts_bu BEFORE UPDATE ON session_events
BEGIN
  INSERT INTO event_search_fts(event_search_fts, rowid, content)
  SELECT 'delete', id, content FROM event_search_content
  WHERE thread_id = old.thread_id AND seq = old.seq;
  DELETE FROM event_search_docs WHERE thread_id = old.thread_id AND seq = old.seq;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER event_search_fts_au AFTER UPDATE ON session_events
WHEN new.type = 'acp_message' AND trim(new.content) <> ''
  AND json_valid(new.acp) AND json_extract(new.acp, '$.id') = new.thread_id
BEGIN
  INSERT INTO event_search_docs(thread_id, seq) VALUES (new.thread_id, new.seq);
  INSERT INTO event_search_fts(rowid, content)
  SELECT id, content FROM event_search_content
  WHERE thread_id = new.thread_id AND seq = new.seq;
END;
-- +goose StatementEnd

DROP TRIGGER thread_search_fts_ai;
DROP TRIGGER thread_search_fts_bd;
DROP TRIGGER thread_search_fts_bu;
DROP TRIGGER thread_search_fts_au;
DROP TABLE thread_search_fts;
DROP VIEW thread_search_content;

CREATE VIEW thread_search_content AS
SELECT
  d.id,
  d.thread_id,
  coalesce(t.title, '') AS title,
  t.slug,
  coalesce(t.project_path, '') AS project_path
FROM thread_search_docs d
JOIN threads t ON t.id = d.thread_id;

CREATE VIRTUAL TABLE thread_search_fts USING fts5(
  title,
  slug,
  project_path,
  content='thread_search_content',
  content_rowid='id',
  tokenize='unicode61',
  prefix='2 3 4'
);
INSERT INTO thread_search_fts(thread_search_fts) VALUES ('rebuild');

-- +goose StatementBegin
CREATE TRIGGER thread_search_fts_ai AFTER INSERT ON threads
BEGIN
  INSERT INTO thread_search_docs(thread_id) VALUES (new.id);
  INSERT INTO thread_search_fts(rowid, title, slug, project_path)
  SELECT id, title, slug, project_path FROM thread_search_content
  WHERE thread_id = new.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER thread_search_fts_bd BEFORE DELETE ON threads
BEGIN
  INSERT INTO thread_search_fts(thread_search_fts, rowid, title, slug, project_path)
  SELECT 'delete', id, title, slug, project_path FROM thread_search_content
  WHERE thread_id = old.id;
  DELETE FROM thread_search_docs WHERE thread_id = old.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER thread_search_fts_bu BEFORE UPDATE OF title, slug, project_path ON threads
BEGIN
  INSERT INTO thread_search_fts(thread_search_fts, rowid, title, slug, project_path)
  SELECT 'delete', id, title, slug, project_path FROM thread_search_content
  WHERE thread_id = old.id;
  DELETE FROM thread_search_docs WHERE thread_id = old.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER thread_search_fts_au AFTER UPDATE OF title, slug, project_path ON threads
BEGIN
  INSERT INTO thread_search_docs(thread_id) VALUES (new.id);
  INSERT INTO thread_search_fts(rowid, title, slug, project_path)
  SELECT id, title, slug, project_path FROM thread_search_content
  WHERE thread_id = new.id;
END;
-- +goose StatementEnd
