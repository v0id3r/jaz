-- +goose Up
-- seq places an event in history and never changes; stream_seq orders writes
-- for the live stream and advances whenever a coalesced event is updated.
ALTER TABLE session_events ADD COLUMN stream_seq INTEGER NOT NULL DEFAULT 0;

-- The backfill leaves search content unchanged, so it skips reindexing.
DROP TRIGGER event_search_fts_bu;
DROP TRIGGER event_search_fts_au;
UPDATE session_events SET stream_seq = seq;

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

CREATE INDEX idx_session_events_stream ON session_events(thread_id, stream_seq);

-- +goose Down
DROP INDEX idx_session_events_stream;
ALTER TABLE session_events DROP COLUMN stream_seq;
