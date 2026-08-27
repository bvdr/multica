package main

import (
	"go/format"
	"path/filepath"
	"testing"
)

func TestExecenvAssertionsStayCollapsed(t *testing.T) {
	t.Parallel()

	result, err := rewritePaths([]string{
		filepath.Join("..", "..", "internal", "daemon", "execenv"),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.blocks != 0 {
		t.Fatalf("execenv has %d collapsible assertion blocks in %d files; run go run ./cmd/rewrite-test-assertions -w ./internal/daemon/execenv", result.blocks, result.files)
	}
	const maxHandwritten = 1236
	if result.handwritten > maxHandwritten {
		t.Fatalf("execenv has %d hand-written assertion blocks, maximum is %d", result.handwritten, maxHandwritten)
	}
}

func TestRewriteSourcePreservesTestingMethodAndLazyArguments(t *testing.T) {
	t.Parallel()

	input := `package sample
import "testing"
func test(t *testing.T, got, want int, err error) {
	if got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got == 0 {
		t.Fatal(expensiveMessage())
	}
	if got < 0 {
		t.Error("negative")
	}
}
`
	want := `package sample
import (
	"testing"

	testassert "github.com/multica-ai/multica/server/internal/testutil"
)
func test(t *testing.T, got, want int, err error) {
	testassert.OnFailure(t, got != want, func() { t.Fatalf("got %d, want %d", got, want) })
	testassert.OnFailure(t, err != nil, func() { t.Errorf("unexpected error: %v", err) })
	testassert.OnFailure(t, got == 0, func() { t.Fatal(expensiveMessage()) })
	testassert.OnFailure(t, got < 0, func() { t.Error("negative") })
}
`
	assertRewrite(t, input, want, 4)
}

func TestRewriteSourceUsesExistingImportAndIsIdempotent(t *testing.T) {
	t.Parallel()

	input := `package sample
import (
	"testing"
	"github.com/multica-ai/multica/server/internal/testutil"
)
func test(t *testing.T, failed bool) {
	if failed {
		t.Fatal("failed")
	}
}
`
	want := `package sample
import (
	"testing"
	"github.com/multica-ai/multica/server/internal/testutil"
)
func test(t *testing.T, failed bool) {
	testutil.OnFailure(t, failed, func() { t.Fatal("failed") })
}
`
	rewritten := assertRewrite(t, input, want, 1)
	again, blocks, handwritten, err := rewriteSource("sample_test.go", rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if blocks != 0 {
		t.Fatalf("second rewrite found %d blocks", blocks)
	}
	if string(again) != string(rewritten) {
		t.Fatal("second rewrite changed the source")
	}
	if handwritten != 0 {
		t.Fatalf("second rewrite found %d hand-written blocks", handwritten)
	}
}

func TestRewriteSourceSkipsScopesAndCommentsItCannotPreserve(t *testing.T) {
	t.Parallel()

	input := `package sample
import "testing"
func test(t *testing.T, failed bool) {
	if got := compute(); got != 0 {
		t.Fatalf("got %d", got)
	}
	if failed {
		// This explanation belongs to the failure.
		t.Error("failed")
	}
	if failed {
		t.Error("failed")
	} else {
		work()
	}
	if ready {
		work()
	} else if failed {
		t.Fatal("failed")
	}
}
`
	assertRewrite(t, input, input, 0)
}

func assertRewrite(t *testing.T, input, want string, wantBlocks int) []byte {
	t.Helper()
	rewritten, blocks, handwritten, err := rewriteSource("sample_test.go", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if blocks != wantBlocks {
		t.Fatalf("blocks = %d, want %d", blocks, wantBlocks)
	}
	if handwritten < blocks {
		t.Fatalf("hand-written blocks = %d, want at least %d", handwritten, blocks)
	}
	wantFormatted := []byte(want)
	if wantBlocks > 0 {
		wantFormatted, err = format.Source(wantFormatted)
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(rewritten) != string(wantFormatted) {
		t.Fatalf("rewrite mismatch\ngot:\n%s\nwant:\n%s", rewritten, wantFormatted)
	}
	return rewritten
}
