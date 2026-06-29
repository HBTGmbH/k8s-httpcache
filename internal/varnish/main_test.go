package varnish

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs every test in this package under goleak so a goroutine left
// running by the process/prefix-writer machinery fails the suite.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
