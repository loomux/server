-- Read-only audit for orphan rows created while foreign_keys was not enforced
-- on every pooled connection (LOOM-67).
--
-- Run against the live Loomux SQLite database with any SQLite client, e.g.:
--   sqlite3 /var/lib/loomux/loomux.db < docs/sql/loom-67-orphan-check.sql
--
-- This script only executes SELECTs; it will not modify the database.

-- Workspaces referencing a missing target.
SELECT 'workspace_with_missing_target' AS check_name,
       COUNT(*) AS orphan_count
FROM workspaces
WHERE target_id NOT IN (SELECT id FROM targets);

-- Tasks referencing a missing workspace.
SELECT 'task_with_missing_workspace' AS check_name,
       COUNT(*) AS orphan_count
FROM tasks
WHERE workspace_id NOT IN (SELECT id FROM workspaces);

-- Credentials scoped to a workspace that no longer exists.
-- workspace_id is nullable (global credentials), so exclude NULL.
SELECT 'credential_with_missing_workspace' AS check_name,
       COUNT(*) AS orphan_count
FROM credentials
WHERE workspace_id IS NOT NULL
  AND workspace_id NOT IN (SELECT id FROM workspaces);

-- Messages linked to a task that no longer exists.
-- task_id is nullable (messages can exist without a task), so exclude NULL.
SELECT 'message_with_missing_task' AS check_name,
       COUNT(*) AS orphan_count
FROM messages
WHERE task_id IS NOT NULL
  AND task_id NOT IN (SELECT id FROM tasks);

-- Per-row detail for any orphans found. Replace the check_name filter to
-- inspect a specific category.
SELECT 'detail_workspace_with_missing_target' AS check_name,
       w.id AS workspace_id,
       w.name AS workspace_name,
       w.target_id AS missing_target_id
FROM workspaces w
WHERE w.target_id NOT IN (SELECT id FROM targets)
UNION ALL
SELECT 'detail_task_with_missing_workspace' AS check_name,
       t.id AS task_id,
       t.workspace_id AS missing_workspace_id,
       NULL
FROM tasks t
WHERE t.workspace_id NOT IN (SELECT id FROM workspaces)
UNION ALL
SELECT 'detail_credential_with_missing_workspace' AS check_name,
       c.id AS credential_id,
       c.name AS credential_name,
       c.workspace_id AS missing_workspace_id
FROM credentials c
WHERE c.workspace_id IS NOT NULL
  AND c.workspace_id NOT IN (SELECT id FROM workspaces)
UNION ALL
SELECT 'detail_message_with_missing_task' AS check_name,
       m.id AS message_id,
       m.conversation_id,
       m.task_id AS missing_task_id
FROM messages m
WHERE m.task_id IS NOT NULL
  AND m.task_id NOT IN (SELECT id FROM tasks);
