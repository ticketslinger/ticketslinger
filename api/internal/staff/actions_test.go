package staff_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/staff"
)

func TestPurposeNamesKindOrganizerAndEvent(t *testing.T) {
	org, ev := uuid.New(), uuid.New()
	if got := staff.Purpose("suspend", org, nil); got != "suspend:"+org.String() {
		t.Errorf("suspend: %q", got)
	}
	if got := staff.Purpose("refund_event", org, &ev); got != "refund_event:"+org.String()+":"+ev.String() {
		t.Errorf("refund_event: %q", got)
	}
}

func TestActSuspendsLiftsAndAlerts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	sess, err := f.svc.Start(ctx, f.user, f.login, "acme", "Reported for selling fake tickets", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const why = "Selling tickets to an illegal event"
	alert := func(subject string) (text string) {
		t.Helper()
		var payload []byte
		if err := f.owner.QueryRow(ctx, `select payload from jobs where kind = 'send_email' and payload->>'subject' = $1`, subject).Scan(&payload); err != nil {
			t.Fatalf("%s: %v", subject, err)
		}
		var mail struct{ To, Text string }
		_ = json.Unmarshal(payload, &mail)
		if mail.To != alertTo || !strings.Contains(mail.Text, "s@example.com") || !strings.Contains(mail.Text, why) || !strings.Contains(mail.Text, "UTC") {
			t.Fatalf("alert %q: %+v", subject, mail)
		}
		return mail.Text
	}

	if _, err := staff.New(f.pool, "", nil).Act(ctx, sess, "suspend", nil, why); !errors.Is(err, staff.ErrAlertsOff) {
		t.Fatalf("no alert address: %v", err)
	}
	a, err := f.svc.Act(ctx, sess, "suspend", nil, why)
	if err != nil || a.Kind != "suspend" || a.OrganizerSlug != "acme" || a.ID == uuid.Nil {
		t.Fatalf("suspend: %+v %v", a, err)
	}
	if text := alert("Staff suspended acme"); !strings.Contains(text, "Report the account to Stripe support") {
		t.Errorf("no owner steps: %s", text)
	}
	if _, err := f.svc.Act(ctx, sess, "suspend", nil, why); !errors.Is(err, staff.ErrAlreadySuspended) {
		t.Fatalf("suspend twice: %v", err)
	}
	ev := uuid.New()
	if _, err := f.svc.Act(ctx, sess, "refund_event", &ev, why); !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("refund of no event: %v", err)
	}
	if _, err := f.svc.Act(ctx, sess, "lift", nil, why); err != nil {
		t.Fatal(err)
	}
	if text := alert("Staff lifted the suspension of acme"); strings.Contains(text, "Stripe") {
		t.Errorf("owner steps on a lift: %s", text)
	}
	if _, err := f.svc.Act(ctx, sess, "lift", nil, why); !errors.Is(err, staff.ErrNotSuspended) {
		t.Fatalf("lift twice: %v", err)
	}
	if err := f.svc.End(ctx, f.user, f.login, "staff", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Act(ctx, sess, "suspend", nil, why); !errors.Is(err, staff.ErrNotAllowed) {
		t.Fatalf("after staff mode ended: %v", err)
	}
	if n := f.count(t, `select count(*) from staff_actions`); n != 2 {
		t.Fatalf("staff_actions = %d, want 2", n)
	}
	list, err := f.svc.List(ctx, f.user)
	if err != nil || len(list) != 1 || len(list[0].Actions) != 2 || list[0].Actions[0].Kind != "suspend" || list[0].Actions[1].Reason != why {
		t.Fatalf("list: %+v %v", list, err)
	}
}

// refund_event: the refund runs after the action is recorded, and a refund
// that fails leaves the action recorded and unfinished.
func TestActRefundsTheEvent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	var ev uuid.UUID
	if err := f.owner.QueryRow(ctx, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at)
		values ($1, 'gala', 'Gala', 'UTC', now() + interval '1 day', now() + interval '2 days') returning id`, f.org).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	var called []uuid.UUID
	fail := error(nil)
	svc := staff.New(f.pool, alertTo, func(_ context.Context, org, action uuid.UUID) (staff.RefundResult, error) {
		if org != f.org {
			t.Errorf("refund for organizer %v", org)
		}
		called = append(called, action)
		return staff.RefundResult{Orders: 2, PendingOrders: 1, AmountCents: 7800, FeeCents: 300}, fail
	})
	sess, err := svc.Start(ctx, f.user, f.login, "acme", "Reported for selling fake tickets", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const why = "Refunding the buyers of an illegal event"
	if _, err := svc.Act(ctx, sess, "suspend", nil, why); err != nil {
		t.Fatal(err)
	}
	a, err := svc.Act(ctx, sess, "refund_event", &ev, why)
	if err != nil || len(called) != 1 || called[0] != a.ID || a.Result == nil || *a.Result != (staff.RefundResult{Orders: 2, PendingOrders: 1, AmountCents: 7800, FeeCents: 300}) {
		t.Fatalf("refund: %+v %v %v", a, err, called)
	}
	fail = errors.New("boom")
	if _, err := svc.Act(ctx, sess, "refund_event", &ev, why); err == nil || len(called) != 2 {
		t.Fatalf("a failed refund: %v", err)
	}
	fail = &pgconn.PgError{Code: "55000", Message: "already_done"}
	if _, err := svc.Act(ctx, sess, "refund_event", &ev, why); !errors.Is(err, staff.ErrAlreadyDone) {
		t.Fatalf("already done: %v", err)
	}
	if n := f.count(t, `select count(*) from staff_actions where kind = 'refund_event'`); n != 3 {
		t.Fatalf("refund_event actions = %d, want all recorded", n)
	}
}
