-- LOOM-316: how each agent CLI is signed in on a target. The probe's auth
-- check also yields the method, the plan and when the sign-in ends (short
-- allow-listed words, never the account's email). Turns record the sign-in
-- screen or usage limit they last hit, and Loomux records the tmux session
-- it opened for a person to sign the CLI in. The probe's upsert never
-- touches the turn or sign-in columns.

-- +goose Up
ALTER TABLE target_agents ADD COLUMN auth_method TEXT NOT NULL DEFAULT '';
ALTER TABLE target_agents ADD COLUMN auth_plan TEXT NOT NULL DEFAULT '';
ALTER TABLE target_agents ADD COLUMN auth_expires_at TIMESTAMP;
ALTER TABLE target_agents ADD COLUMN sign_in_problem TEXT NOT NULL DEFAULT '';
ALTER TABLE target_agents ADD COLUMN sign_in_problem_at TIMESTAMP;
ALTER TABLE target_agents ADD COLUMN limited_at TIMESTAMP;
ALTER TABLE target_agents ADD COLUMN limit_resets TEXT NOT NULL DEFAULT '';
ALTER TABLE target_agents ADD COLUMN sign_in_session TEXT NOT NULL DEFAULT '';
ALTER TABLE target_agents ADD COLUMN sign_in_started_at TIMESTAMP;

-- +goose Down
ALTER TABLE target_agents DROP COLUMN sign_in_started_at;
ALTER TABLE target_agents DROP COLUMN sign_in_session;
ALTER TABLE target_agents DROP COLUMN limit_resets;
ALTER TABLE target_agents DROP COLUMN limited_at;
ALTER TABLE target_agents DROP COLUMN sign_in_problem_at;
ALTER TABLE target_agents DROP COLUMN sign_in_problem;
ALTER TABLE target_agents DROP COLUMN auth_expires_at;
ALTER TABLE target_agents DROP COLUMN auth_plan;
ALTER TABLE target_agents DROP COLUMN auth_method;
