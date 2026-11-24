package aggregate

import (
	"exectop/src/runner"
	"exectop/src/status"
	"testing"
)

func TestExitFailOnWarnKeepsSuccessfulRunsSuccessful(t *testing.T) {
	if got := Exit([]runner.Result{{Status: status.OK}}, "warn"); got != 0 {
		t.Fatalf("all OK exit=%d", got)
	}
	if got := Exit([]runner.Result{{Status: status.Skip}}, "warn"); got != 0 {
		t.Fatalf("all SKIP exit=%d", got)
	}
	if got := Exit([]runner.Result{{Status: status.Warn}}, "warn"); got != 1 {
		t.Fatalf("WARN exit=%d", got)
	}
	if got := Exit([]runner.Result{{Status: status.Fail}}, "warn"); got != 1 {
		t.Fatalf("FAIL exit=%d", got)
	}
}

func TestVanishedIsFailure(t *testing.T) {
	if got := Exit([]runner.Result{{Status: status.Vanished}}, "fail"); got != 1 {
		t.Fatalf("VANISHED exit=%d, want 1", got)
	}
	if !status.IsFailure(status.Vanished) || status.Vanished.Severity() != status.Fail.Severity() {
		t.Fatalf("VANISHED failure classification is inconsistent")
	}
}
