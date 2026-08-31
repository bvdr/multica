package handler

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// runtimeLookup builds the agent_runtime reader for one product source, for
// handlers that hand the reader to a shared readiness helper rather than
// reading the row themselves.
//
// The source labels multica_agent_runtime_lookup_total (MUL-6884). Pick the
// obsmetrics.RuntimeLookupSource* constant that names the product behaviour
// driving the read, not the file the call happens to live in: a poll loop
// counted as generic API traffic is exactly the confusion the metric exists to
// remove.
func (h *Handler) runtimeLookup(source string) service.RuntimeLookup {
	return service.RuntimeLookup{Queries: h.Queries, Metrics: h.Metrics, Source: source}
}

// getAgentRuntime reads one agent_runtime row and attributes it to source.
func (h *Handler) getAgentRuntime(ctx context.Context, source string, id pgtype.UUID) (db.AgentRuntime, error) {
	return h.runtimeLookup(source).Get(ctx, id)
}
