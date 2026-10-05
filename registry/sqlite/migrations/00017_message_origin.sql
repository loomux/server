-- Where a turn ran, recorded when it's logged (registry.Message.Origin):
-- the purpose of the target it acted on (personal or work), 'none' for a
-- turn that touched no target, '' when unknown. A fresh agent is only
-- shown earlier turns of its own target's purpose, and task_id can't say
-- where a turn ran once its workspace is deleted (ON DELETE SET NULL).
--
-- Existing messages still linked to a task get their target's purpose;
-- the rest stay unknown, which a fresh agent is not shown.

-- +goose Up
ALTER TABLE messages ADD COLUMN origin TEXT NOT NULL DEFAULT '';
UPDATE messages SET origin = (
    SELECT CASE WHEN targets.purpose = '' THEN 'personal' ELSE targets.purpose END
    FROM tasks
    JOIN workspaces ON workspaces.id = tasks.workspace_id
    JOIN targets ON targets.id = workspaces.target_id
    WHERE tasks.id = messages.task_id
)
WHERE task_id IS NOT NULL AND EXISTS (
    SELECT 1 FROM tasks
    JOIN workspaces ON workspaces.id = tasks.workspace_id
    JOIN targets ON targets.id = workspaces.target_id
    WHERE tasks.id = messages.task_id
);

-- +goose Down
ALTER TABLE messages DROP COLUMN origin;
