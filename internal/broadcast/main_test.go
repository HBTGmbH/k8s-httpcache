package broadcast

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under goleak: the broadcast server
// starts real listeners, per-connection goroutines, and a keep-alive client
// pool, and a test leaking any of those (e.g. a forgotten Shutdown) should
// fail the suite rather than pass silently.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
