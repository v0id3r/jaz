-- +goose Up
-- search_terms() now stems English words too.
INSERT INTO message_search_fts(message_search_fts) VALUES ('rebuild');
INSERT INTO event_search_fts(event_search_fts) VALUES ('rebuild');
INSERT INTO thread_search_fts(thread_search_fts) VALUES ('rebuild');

-- +goose Down
INSERT INTO message_search_fts(message_search_fts) VALUES ('rebuild');
INSERT INTO event_search_fts(event_search_fts) VALUES ('rebuild');
INSERT INTO thread_search_fts(thread_search_fts) VALUES ('rebuild');
