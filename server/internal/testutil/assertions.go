package testutil

type helperTB interface {
	Helper()
}

// OnFailure runs report only when failed is true. Keeping the original
// testing call inside report preserves Fatal versus Error control flow, the
// exact diagnostic, and lazy evaluation of diagnostic arguments.
func OnFailure(t helperTB, failed bool, report func()) {
	t.Helper()
	if failed {
		report()
	}
}
