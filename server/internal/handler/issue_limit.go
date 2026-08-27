package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/service"
)

const (
	issueLimitModeLimited     = "limited"
	issueLimitModeUnlimited   = "unlimited"
	issueLimitModeUnavailable = "unavailable"
)

type IssueLimitUsageResponse struct {
	Mode           string  `json:"mode"`
	Used           *int64  `json:"used,omitempty"`
	Limit          *int64  `json:"limit,omitempty"`
	Reached        *bool   `json:"reached,omitempty"`
	HasMore        *bool   `json:"has_more,omitempty"`
	PolicyRevision *int64  `json:"policy_revision,omitempty"`
	CalculatedAt   *string `json:"calculated_at,omitempty"`
}

// GetIssueLimitUsage combines Cloud's effective instruction with Multica's
// local issue-row count. The response is already display-ready: clients do not
// compare used and limit or infer unlimited access from subscription fields.
func (h *Handler) GetIssueLimitUsage(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	policy := service.ResolveIssueCountPolicy(r.Context(), h.Entitlements, workspaceID)
	if policy.Action == entitlement.ActionOff {
		mode := issueLimitModeUnavailable
		if policy.Reason == entitlement.ReasonCacheFresh || policy.Reason == entitlement.ReasonRefreshed {
			mode = issueLimitModeUnlimited
		}
		writeJSON(w, http.StatusOK, IssueLimitUsageResponse{Mode: mode})
		return
	}
	if policy.Action != entitlement.ActionEnforce {
		writeJSON(w, http.StatusOK, IssueLimitUsageResponse{Mode: issueLimitModeUnavailable})
		return
	}
	used, reached, hasMore, err := service.CountIssueUsage(r.Context(), h.Queries, workspaceID, policy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load issue limit usage")
		return
	}
	calculatedAt := time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, IssueLimitUsageResponse{
		Mode: issueLimitModeLimited, Used: &used, Limit: &policy.Limit,
		Reached: &reached, HasMore: &hasMore, PolicyRevision: &policy.PolicyRevision,
		CalculatedAt: &calculatedAt,
	})
}

func writeIssueLimitReached(w http.ResponseWriter, err error) bool {
	var limitErr *service.IssueLimitReachedError
	if !errors.As(err, &limitErr) {
		return false
	}
	writeJSON(w, http.StatusPaymentRequired, map[string]any{
		"code":            "issue_limit_reached",
		"error":           "workspace has reached its issue limit",
		"limit":           limitErr.Limit,
		"policy_revision": limitErr.PolicyRevision,
	})
	return true
}
