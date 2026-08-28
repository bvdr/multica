-- Resolve CI webhook head-SHA fallbacks without scanning every mirrored PR,
-- while keeping PR-number address lookups on the same installation/repo key.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_github_pull_request_installation_repo_head_pr
    ON github_pull_request (installation_id, repo_owner, repo_name, head_sha, pr_number);
