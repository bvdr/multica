package execenv

import (
	"strings"
	"testing"

	testassert "github.com/multica-ai/multica/server/internal/testutil"
)

// TestRenderIssueContext_HandoffNote verifies the handoff note lands in
// issue_context.md under its own section, distinct from the comment-reply
// trigger framing.
func TestRenderIssueContext_HandoffNote(t *testing.T) {
	note := "Scope to the auth module only."
	md := renderIssueContext("claude", TaskContextForEnv{IssueID: "issue-1", HandoffNote: note})

	testassert.OnFailure(t, !strings.Contains(md, "## Handoff Note"), func() { t.Fatalf("expected Handoff Note section:\n%s", md) })
	testassert.OnFailure(t, !strings.Contains(md, note), func() { t.Fatalf("handoff note text missing:\n%s", md) })
	testassert.OnFailure(t, !strings.Contains(md, "**Trigger:** New Assignment"), func() { t.Fatalf("handoff note must render under the assignment trigger:\n%s", md) })
}

// TestRenderIssueContext_NoHandoffNote keeps the assignment context clean when
// no note is present.
func TestRenderIssueContext_NoHandoffNote(t *testing.T) {
	md := renderIssueContext("claude", TaskContextForEnv{IssueID: "issue-1"})
	testassert.OnFailure(t, strings.Contains(md, "## Handoff Note"), func() { t.Fatalf("unexpected Handoff Note section when no note set:\n%s", md) })
}
