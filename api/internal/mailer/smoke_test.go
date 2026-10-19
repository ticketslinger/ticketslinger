package mailer_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/emails"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

// TestResendSmoke sends real email through Resend, to Resend's own test
// address only. It runs only from the manual mail-smoke workflow.
func TestResendSmoke(t *testing.T) {
	key := os.Getenv("TS_RESEND_SMOKE_KEY")
	if key == "" {
		t.Skip("TS_RESEND_SMOKE_KEY not set")
	}
	if !strings.HasPrefix(key, "re_") {
		t.Fatal("TS_RESEND_SMOKE_KEY isn't a Resend key")
	}
	const to = "delivered@resend.dev"
	rb := func(n int) []byte {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	d := emails.TicketsData{
		To: to, Event: "Smoke Test Night", When: "Sat 1 Jan, 19:00", Venue: "Nowhere Hall", Organizer: "TicketSlinger",
		Code: "TS-SMOKE", OrderURL: "https://ticketslinger.com/orders/TS-SMOKE", LostLinkURL: "https://ticketslinger.com/orders",
		OrderID: uuid.New(), OrganizerID: uuid.New(),
	}
	for i := range 3 {
		d.Tickets = append(d.Tickets, emails.Ticket{Type: "General", Attendee: "Guest " + string(rune('A'+i)), QR: base64.RawURLEncoding.EncodeToString(rb(16))})
	}
	msg, err := emails.Tickets(d)
	if err != nil {
		t.Fatal(err)
	}
	run := os.Getenv("GITHUB_RUN_ID")
	if run == "" {
		run = hex.EncodeToString(rb(8))
	}
	msg.IdempotencyKey = "ts-smoke-" + run

	// The first send's counts: the one answer that must carry Resend's quota headers.
	daily, monthly := -1, -1
	calls := 0
	r := &mailer.Resend{Key: key, From: "TicketSlinger <tickets@ticketslinger.com>", URL: "https://api.resend.com",
		Client: &http.Client{Timeout: 15 * time.Second, Transport: headerNames{t}},
		OnQuota: func(_ context.Context, d, m int) {
			calls++
			t.Logf("call %d quota: daily %d, monthly %d", calls, d, m)
			if calls == 1 {
				daily, monthly = d, m
			}
		}}
	ctx := context.Background()
	if err := r.Send(ctx, msg); err != nil {
		t.Fatalf("first send: %v", err)
	}
	t.Log("sent")
	if err := r.Send(ctx, msg); err != nil {
		t.Fatalf("replay: %v", err)
	}
	t.Log("replayed")
	bad := emails.Plain("not-an-address", "smoke", "x")
	var me *mailer.Error
	if err := r.Send(ctx, bad); !errors.As(err, &me) || me.Kind != mailer.Permanent {
		t.Fatalf("bad address: want a Permanent *mailer.Error, got %v", err)
	}
	t.Log("refused as expected")
	if monthly < 1 {
		t.Fatalf("OnQuota monthly = %d, want >= 1", monthly)
	}
	t.Logf("first send: daily %d, monthly %d", daily, monthly)
}

// headerNames logs the names, never the values, of Resend's quota and rate
// headers on every answer.
type headerNames struct{ t *testing.T }

func (h headerNames) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	var names []string
	for k := range res.Header {
		if l := strings.ToLower(k); strings.HasPrefix(l, "x-resend") || strings.HasPrefix(l, "ratelimit") {
			names = append(names, l)
		}
	}
	slices.Sort(names)
	h.t.Logf("%d answer headers: %v", res.StatusCode, names)
	return res, nil
}
