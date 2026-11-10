-- LOOM-268: who a session is. label and client_kind say which device and
-- client hold it, for the sessions page; origin says how it was made:
-- 'login' with the password, or 'link' approved from another session (a
-- terminal). Every session so far was made by a login, so existing rows
-- become 'login': they can approve a link without logging in again. Their
-- client kind isn't known and stays empty.

-- +goose Up
ALTER TABLE sessions ADD COLUMN label TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN client_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN origin TEXT NOT NULL DEFAULT '';
UPDATE sessions SET origin = 'login';

-- +goose Down
ALTER TABLE sessions DROP COLUMN origin;
ALTER TABLE sessions DROP COLUMN client_kind;
ALTER TABLE sessions DROP COLUMN label;
