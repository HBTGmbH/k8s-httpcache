package watcher

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under goleak so a goroutine left
// running by a watcher (informers, poll loops, fan-in) fails the suite. The
// tests cancel their contexts and the goroutines exit cleanly within goleak's
// retry window, so no framework ignores are needed.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
