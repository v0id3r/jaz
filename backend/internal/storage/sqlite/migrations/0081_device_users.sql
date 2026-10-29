-- +goose Up
ALTER TABLE devices ADD COLUMN user_identity TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE devices DROP COLUMN user_identity;
