package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func clearWorkspaceDefaultLocalDirectory(t *testing.T) {
	t.Helper()
	req := newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{"default_local_directory": nil})
	req = withURLParam(req, "id", testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusOK)
}

func getWorkspaceResponse(t *testing.T) WorkspaceResponse {
	t.Helper()
	req := withURLParam(newRequest("GET", "/api/workspaces/"+testWorkspaceID, nil), "id", testWorkspaceID)
	var out WorkspaceResponse
	testutil.Call(t, testHandler.GetWorkspace, req).Want(http.StatusOK).JSON(&out)
	return out
}

func TestUpdateWorkspaceDefaultLocalDirectoryRoundTrip(t *testing.T) {
	t.Cleanup(func() { clearWorkspaceDefaultLocalDirectory(t) })

	req := newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{
		"default_local_directory": map[string]any{
			"local_path": "/Users/dev/contextpro", "daemon_id": "daemon-roundtrip",
			"execution_mode": "in_place", "label": "ContextPRO",
		},
	})
	req = withURLParam(req, "id", testWorkspaceID)
	var updated WorkspaceResponse
	testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusOK).JSON(&updated)
	got, ok := updated.DefaultLocalDirectory.(map[string]any)
	if !ok || got["local_path"] != "/Users/dev/contextpro" || got["execution_mode"] != "in_place" || got["label"] != "ContextPRO" {
		t.Fatalf("PUT response default_local_directory = %#v", updated.DefaultLocalDirectory)
	}

	if fetched := getWorkspaceResponse(t); fetched.DefaultLocalDirectory == nil {
		t.Fatal("GET dropped default_local_directory after it was saved")
	}

	clearWorkspaceDefaultLocalDirectory(t)
	if fetched := getWorkspaceResponse(t); fetched.DefaultLocalDirectory != nil {
		t.Fatalf("null did not clear the default, got %#v", fetched.DefaultLocalDirectory)
	}
}

func TestUpdateWorkspaceRejectsInvalidDefaultLocalDirectory(t *testing.T) {
	for name, ref := range map[string]map[string]any{
		"relative path":  {"local_path": "relative/dir", "daemon_id": "d"},
		"missing daemon": {"local_path": "/srv/app"},
		"unknown mode":   {"local_path": "/srv/app", "daemon_id": "d", "execution_mode": "screen"},
	} {
		t.Run(name, func(t *testing.T) {
			req := newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{"default_local_directory": ref})
			req = withURLParam(req, "id", testWorkspaceID)
			testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusBadRequest)
		})
	}
	if fetched := getWorkspaceResponse(t); fetched.DefaultLocalDirectory != nil {
		t.Fatalf("a rejected update still stored a default: %#v", fetched.DefaultLocalDirectory)
	}
}
