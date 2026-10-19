package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

// SetNow pins the clock Deliver reads until the test ends.
func SetNow(t *testing.T, at time.Time) {
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

type quotaMailer struct{}

func (quotaMailer) Send(context.Context, mailer.Message) error {
	return &mailer.Error{Provider: "resend", Kind: mailer.DailyQuota, Status: 429, Code: "daily_quota_exceeded"}
}

// DailyQuotaUntil is when Deliver, at the pinned time at, asks a job that hit
// the daily quota to run again.
func DailyQuotaUntil(t *testing.T, at time.Time) time.Time {
	SetNow(t, at)
	var d delayError
	if !errors.As(Deliver(context.Background(), quotaMailer{}, mailer.Message{}), &d) {
		t.Fatal("not a delay")
	}
	return d.until
}
