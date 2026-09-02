package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestResolveClaimProjectContextSynthesizesWorkspaceDefault(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { clearWorkspaceDefaultLocalDirectory(t) })

	// A project with no resources of its own.
	req := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{"title": "tmux default folder project"})
	var project ProjectResponse
	testutil.Call(t, testHandler.CreateProject, req).Want(http.StatusCreated).JSON(&project)
	t.Cleanup(func() {
		del := withURLParam(newRequest("DELETE", "/api/projects/"+project.ID, nil), "id", project.ID)
		testHandler.DeleteProject(httptest.NewRecorder(), del)
	})

	// No default yet: no local_directory resource is attached.
	out, err := testHandler.resolveClaimProjectContext(ctx, parseUUID(project.ID), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("resolve without default: %v", err)
	}
	if hasLocalDirectoryResource(out.Resources) {
		t.Fatalf("no default configured but a local_directory resource appeared: %+v", out.Resources)
	}

	// Workspace default set: it is synthesized as a resource.
	setReq := newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{
		"default_local_directory": map[string]any{"local_path": "/Users/dev/default", "daemon_id": "daemon-default", "execution_mode": "in_place"},
	})
	testutil.Call(t, testHandler.UpdateWorkspace, withURLParam(setReq, "id", testWorkspaceID)).Want(http.StatusOK)

	out, err = testHandler.resolveClaimProjectContext(ctx, parseUUID(project.ID), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("resolve with default: %v", err)
	}
	var synthesized *ProjectResourceData
	for i := range out.Resources {
		if out.Resources[i].ResourceType == "local_directory" {
			synthesized = &out.Resources[i]
		}
	}
	if synthesized == nil || synthesized.ID != workspaceDefaultLocalDirectoryResourceID {
		t.Fatalf("workspace default not synthesized: %+v", out.Resources)
	}
	var ref localDirectoryRef
	if err := json.Unmarshal(synthesized.ResourceRef, &ref); err != nil || ref.LocalPath != "/Users/dev/default" || ref.DaemonID != "daemon-default" {
		t.Fatalf("synthesized ref = %s (%v)", synthesized.ResourceRef, err)
	}

	// A project resource wins over the default.
	resReq := newRequest("POST", "/api/projects/"+project.ID+"/resources", map[string]any{
		"resource_type": "local_directory",
		"resource_ref":  map[string]any{"local_path": "/Users/dev/project-own", "daemon_id": "daemon-own", "execution_mode": "in_place"},
	})
	testutil.Call(t, testHandler.CreateProjectResource, withURLParam(resReq, "id", project.ID)).Want(http.StatusCreated)

	out, err = testHandler.resolveClaimProjectContext(ctx, parseUUID(project.ID), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("resolve with project resource: %v", err)
	}
	var localDirs int
	for _, res := range out.Resources {
		if res.ResourceType != "local_directory" {
			continue
		}
		localDirs++
		if res.ID == workspaceDefaultLocalDirectoryResourceID {
			t.Fatal("workspace default was attached although the project has its own local_directory resource")
		}
	}
	if localDirs != 1 {
		t.Fatalf("expected exactly one local_directory resource, got %d", localDirs)
	}
}
