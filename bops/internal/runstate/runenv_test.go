package runstate

import (
	"os"
	"testing"
)

// BOPS_RUN names the run a command acts as (bops-4ez). A test run under an
// orchestrator that sets it would otherwise put every test in a named run.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("BOPS_RUN")
	os.Exit(m.Run())
}
