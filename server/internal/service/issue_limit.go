package service

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrIssueLimitReached = errors.New("workspace issue limit reached")

// IssueLimitReachedError carries Cloud's effective limit and policy revision
// to transport adapters. It never includes a plan name or subscription state.
type IssueLimitReachedError struct {
	Limit          int64
	PolicyRevision int64
}

func (e *IssueLimitReachedError) Error() string {
	return fmt.Sprintf("%s: limit %d", ErrIssueLimitReached, e.Limit)
}

func (*IssueLimitReachedError) Unwrap() error { return ErrIssueLimitReached }

// IssueCountPolicy is the validated mechanical instruction received from
// Cloud. Resolve it before opening a database transaction so a network call
// never holds product-database locks.
type IssueCountPolicy struct {
	Action         entitlement.Action
	Limit          int64
	PolicyRevision int64
	Reason         entitlement.Reason
}

func ResolveIssueCountPolicy(ctx context.Context, provider entitlement.Provider, workspaceID pgtype.UUID) IssueCountPolicy {
	if provider == nil || !workspaceID.Valid {
		return IssueCountPolicy{Action: entitlement.ActionOff, Reason: entitlement.ReasonDisabled}
	}
	decision := provider.Gate(ctx, uuid.UUID(workspaceID.Bytes), entitlement.GateIssueCount)
	policy := IssueCountPolicy{
		Action:         decision.Gate.Action,
		PolicyRevision: decision.PolicyRevision,
		Reason:         decision.Reason,
	}
	if decision.Gate.Limit != nil {
		policy.Limit = int64(*decision.Gate.Limit)
	}
	switch policy.Action {
	case entitlement.ActionOff:
		policy.Limit = 0
	case entitlement.ActionEnforce, entitlement.ActionObserve:
		if policy.Limit > 0 {
			return policy
		}
		policy.Reason = entitlement.ReasonInvalidPolicy
		policy.Action = entitlement.ActionOff
		policy.Limit = 0
	default:
		policy.Reason = entitlement.ReasonInvalidPolicy
		policy.Action = entitlement.ActionOff
		policy.Limit = 0
	}
	return policy
}

// AllocateIssueNumber serializes creates on the workspace row, then checks the
// current number of issue rows inside the same transaction. The caller must
// roll the transaction back on ErrIssueLimitReached; that also rolls back the
// counter increment. Deleting an issue frees capacity because issue_counter is
// deliberately not used as quota usage.
func AllocateIssueNumber(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, policy IssueCountPolicy) (int32, error) {
	number, err := q.IncrementIssueCounter(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	if policy.Action != entitlement.ActionEnforce {
		return number, nil
	}
	used, err := q.CountIssuesUpTo(ctx, db.CountIssuesUpToParams{
		WorkspaceID: workspaceID,
		Limit:       policy.Limit,
	})
	if err != nil {
		return 0, err
	}
	if used >= policy.Limit {
		return 0, &IssueLimitReachedError{Limit: policy.Limit, PolicyRevision: policy.PolicyRevision}
	}
	return number, nil
}

// CountIssueUsage performs a bounded read for a limited policy. The returned
// used value is exact while below the limit and capped at the limit once the
// workspace is full; hasMore reports rows beyond that cap.
func CountIssueUsage(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, policy IssueCountPolicy) (used int64, reached, hasMore bool, err error) {
	if policy.Action != entitlement.ActionEnforce && policy.Action != entitlement.ActionObserve {
		return 0, false, false, nil
	}
	sampleLimit := policy.Limit
	if sampleLimit < math.MaxInt64 {
		sampleLimit++
	}
	sampled, err := q.CountIssuesUpTo(ctx, db.CountIssuesUpToParams{
		WorkspaceID: workspaceID,
		Limit:       sampleLimit,
	})
	if err != nil {
		return 0, false, false, err
	}
	hasMore = sampled > policy.Limit
	used = sampled
	if hasMore {
		used = policy.Limit
	}
	return used, sampled >= policy.Limit, hasMore, nil
}
