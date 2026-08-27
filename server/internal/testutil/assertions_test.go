package testutil

import "testing"

type helperSpy struct {
	helperCalls int
}

func (s *helperSpy) Helper() {
	s.helperCalls++
}

func TestOnFailurePreservesLazyReporting(t *testing.T) {
	t.Parallel()

	spy := &helperSpy{}
	reports := 0
	OnFailure(spy, false, func() { reports++ })
	if reports != 0 {
		t.Fatalf("false condition ran report %d times", reports)
	}
	OnFailure(spy, true, func() { reports++ })
	if reports != 1 {
		t.Fatalf("true condition ran report %d times", reports)
	}
	if spy.helperCalls != 2 {
		t.Fatalf("Helper calls = %d, want 2", spy.helperCalls)
	}
}
