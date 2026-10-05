-- A v0.1.0 database (schema version 18), as a deployment would hold it:
-- at least one row in every table, with the values that matter set. Raw
-- SQL on purpose: it never changes with the code, so it stays what a
-- v0.1.0 instance wrote (LOOM-126, TestUpgradeFromV010).
INSERT INTO targets (id, name, kind, host, user, ssh_key_ref, created_at, updated_at,
    workspace_root, permission_mode, purpose, allowed_agent_types, no_provision, no_shell, require_confirmation)
VALUES ('t-jet', 'jet01', 'remote', 'jet01', 'orski', '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
    '', 'auto', '', '[]', 0, 0, 0),
       ('t-work', 'sc1', 'remote', 'sc1', 'orski', '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
    '', '', 'work', '["claude-code"]', 0, 1, 1);
INSERT INTO target_agents (target_id, agent_type, available, checked_at, path, version, auth_status)
VALUES ('t-jet', 'claude-code', 1, CURRENT_TIMESTAMP, '/home/orski/.local/bin/claude', '2.1.289 (Claude Code)', 'logged_in');
INSERT INTO target_health (target_id, reachable, error, latency_ms, tmux_version, disk_free_bytes, probed_at)
VALUES ('t-jet', 1, '', 287, 'tmux 3.4', 126990446592, CURRENT_TIMESTAMP);
INSERT INTO workspaces (id, name, path, target_id, git_remote, tags, description, capabilities, status,
    is_dynamic, last_used_at, rolling_summary, created_at, updated_at, status_reason)
VALUES ('w-1', 'e2e-loom56', '/home/orski/loomux-workspaces/e2e-loom56', 't-jet', '', '["e2e"]', 'demo', '[]', 'idle',
    1, CURRENT_TIMESTAMP, 'created hello.txt', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ''),
       ('w-shell', 'shell@jet01', '/home/orski', 't-jet', '', '["loomux:shell"]', '', '[]', 'idle',
    1, NULL, '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '');
INSERT INTO tasks (id, workspace_id, kind, agent_type, tmux_session, status, conversation_id, created_at, updated_at,
    started_at, completed_at, reaped_at, command, exit_code, failure_reason, error_class, output_tail, attention)
VALUES ('k-agent', 'w-1', 'agent', 'claude-code', 'loomux-k-agent', 'completed', 'conv-1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
    CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, NULL, '', NULL, '', '', '', ''),
       ('k-cmd', 'w-shell', 'command', '', 'loomux-k-cmd', 'completed', 'conv-1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
    CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, NULL, 'df -h', 0, '', '', 'Filesystem Size', ''),
       ('k-ask', 'w-1', 'agent', 'claude-code', 'loomux-k-ask', 'needs-attention', 'conv-2', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
    CURRENT_TIMESTAMP, NULL, NULL, '', NULL, '', '', '',
    '{"kind":"permission","title":"Bash command","detail":"rm x","options":[{"label":"Yes"},{"label":"No"}],"selected":0}');
INSERT INTO task_turns (id, task_id, user_message, agent_message, pane, created_at)
VALUES ('tt-1', 'k-agent', 'create hello.txt', 'Created hello.txt.', 'pane text', CURRENT_TIMESTAMP);
INSERT INTO dispatches (id, conversation_id, message, workspace_hint, idempotency_key, request_hash, status,
    reply, error, error_class, created_at, updated_at, started_at, finished_at, confirmation_id)
VALUES ('d-1', 'conv-1', 'how much disk is free on jet01?', '', 'idem-1', 'h1', 'succeeded',
    'I''d run this on jet01', '', '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ''),
       ('d-2', 'conv-1', 'yes', '', NULL, 'h2', 'succeeded',
    'Ran df -h on jet01', '', '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'c-1');
INSERT INTO messages (id, conversation_id, task_id, role, content, created_at, dispatch_id, origin)
VALUES ('m-1', 'conv-1', NULL, 'user', 'how much disk is free on jet01?', CURRENT_TIMESTAMP, 'd-1', ''),
       ('m-2', 'conv-1', NULL, 'assistant', 'I''d run this on jet01', CURRENT_TIMESTAMP, 'd-1', 'personal'),
       ('m-3', 'conv-1', 'k-cmd', 'assistant', 'Ran df -h on jet01', CURRENT_TIMESTAMP, 'd-2', 'personal');
INSERT INTO confirmations (id, conversation_id, dispatch_id, kind, target_id, target_name, agent_type, command,
    workspace, git_remote, status, created_at, expires_at, resolved_at)
VALUES ('c-1', 'conv-1', 'd-1', 'run_command', 't-jet', 'jet01', '', 'df -h', '', '', 'approved',
    CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
INSERT INTO sessions (id, token_hash, created_at, last_used_at)
VALUES ('s-1', 'hash-1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
