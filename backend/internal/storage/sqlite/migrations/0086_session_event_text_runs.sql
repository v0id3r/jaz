-- +goose Up
CREATE INDEX idx_session_events_text_runs ON session_events(thread_id, projection_key, seq)
WHERE projection_op = 'append';

-- +goose Down
DROP INDEX idx_session_events_text_runs;
