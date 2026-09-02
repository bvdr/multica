-- Workspace-level default local directory (ContextPRO fork). Same JSONB ref
-- shape as a project_resource of type local_directory:
-- {local_path, daemon_id, label?, execution_mode?}. A project without its own
-- local_directory resource inherits it at claim time (handler
-- resolveClaimProjectContext). Sits next to repos / mcp_config on the workspace
-- row: read by primary key only, so no index; no foreign key to any runtime
-- (repo rule), the daemon_id is validated in application code.
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS default_local_directory JSONB;
