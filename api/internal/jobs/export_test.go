package jobs

import (
	"testing"
	"time"
)

// SetNow pins the clock Deliver reads until the test ends.
func SetNow(t *testing.T, at time.Time) {
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}
