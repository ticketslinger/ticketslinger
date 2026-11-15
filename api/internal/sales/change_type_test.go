package sales_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func (f fixture) changeType(s *sales.Service, order, ticket, to uuid.UUID) (email string, err error) {
	err = db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		email, err = sales.ChangeTicketType(context.Background(), tx, f.org, f.event, order, ticket, to, audit.Actor{User: f.user})
		return err
	})
	return email, err
}

func (f fixture) used(t *testing.T, q uuid.UUID) int {
	t.Helper()
	return f.count(t, `select quota_used($1, null)`, q)
}

func (f fixture) typeOf(t *testing.T, ticket uuid.UUID) uuid.UUID {
	t.Helper()
	return f.id(t, `select ticket_type_id from tickets where id = $1`, ticket)
}

// mailIdle makes the order's last email old news, so queue_order_email queues.
func (f fixture) mailIdle(t *testing.T, order uuid.UUID) {
	t.Helper()
	f.exec(t, `delete from jobs where kind = 'order_confirmation'`)
	f.exec(t, `update orders set mail_status = 'sent', mail_requested_at = now() - interval '1 hour' where id = $1`, order)
}

func TestChangeType(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	vip := f.ticketType(t, f.org, f.event, "VIP", 2500, false)
	vipQ := f.quota(t, f.org, f.event, "VIP", 5, vip)
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	tk := tickets[0]
	q := f.question(t, "Dietary needs", "text", "ticket", false, nil)
	f.exec(t, `update tickets set attendee_name = 'Ada' where id = $1`, tk)
	f.exec(t, `insert into answers(organizer_id, event_id, order_id, ticket_id, question_id, value) values ($1, $2, $3, $4, $5, 'Vegan')`,
		f.org, f.event, order, tk, q)
	f.mailIdle(t, order)
	secret := f.qrOf(t, tk)
	hall, vipUsed := f.used(t, f.hall), f.used(t, vipQ)

	email, err := f.changeType(s, order, tk, vip)
	f.must(t, err)
	if email != "queued" {
		t.Fatalf("email %q, want queued", email)
	}
	if f.typeOf(t, tk) != vip || f.typeOf(t, tickets[1]) != f.general {
		t.Fatal("the wrong ticket moved")
	}
	if f.qrOf(t, tk) != secret || f.str(t, `select attendee_name from tickets where id = $1`, tk) != "Ada" ||
		f.str(t, `select value from answers where ticket_id = $1`, tk) != "Vegan" {
		t.Fatal("the QR, name or answer changed with the type")
	}
	if f.used(t, f.hall) != hall-1 || f.used(t, vipQ) != vipUsed+1 {
		t.Fatalf("Hall %d→%d, VIP %d→%d: want one seat moved", hall, f.used(t, f.hall), vipUsed, f.used(t, vipQ))
	}
	row := f.str(t, `select before::text || ' ' || after::text from audit_log where action = 'ticket.type_changed' and entity_type = 'ticket' and entity_id = $1`, tk)
	if row != `{"ticket_type_id": "`+f.general.String()+`"} {"ticket_type_id": "`+vip.String()+`"}` {
		t.Fatalf("audit %s", row)
	}
}

// A quota both types count against needs no room: the seat stays in it.
func TestChangeTypeSharedQuota(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500}) // Hall plays Venue
	vip := f.addVIP(t, 2500, false)
	vipQ := f.quota(t, f.org, f.event, "VIP", 1, vip)
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	if f.used(t, f.hall) != 2 {
		t.Fatal("Venue should be full")
	}
	if _, err := f.changeType(s, order, tickets[0], vip); err != nil {
		t.Fatalf("change inside a full shared quota: %v", err)
	}
	if f.used(t, vipQ) != 1 || f.used(t, f.hall) != 2 {
		t.Fatalf("VIP %d, Venue %d: want 1 and 2", f.used(t, vipQ), f.used(t, f.hall))
	}
}

// Review Focus 1: a change and a buyer's checkout race for VIP's one seat.
// Both queue on its quota lock, each queuing first in turn; exactly one wins.
func TestChangeTypeRacesCheckout(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	vip := f.ticketType(t, f.org, f.event, "VIP", 2500, false)
	vipQ := f.quota(t, f.org, f.event, "VIP", 1, vip)
	order, _, tickets := f.paidOrder(t, s, fake, 1, 1)
	ctx := context.Background()
	for round := range 20 {
		lock, err := f.owner.Begin(ctx)
		f.must(t, err)
		_, err = lock.Exec(ctx, `select 1 from quotas where id = $1 for update`, vipQ)
		f.must(t, err)
		var wg sync.WaitGroup
		var changeErr, holdErr error
		change := func() { defer wg.Done(); _, changeErr = f.changeType(s, order, tickets[0], vip) }
		hold := func() {
			defer wg.Done()
			_, holdErr = s.CreateCheckout(ctx, buy(sales.Item{TicketTypeID: vip, Quantity: 1}))
		}
		first, second := change, hold
		if round%2 == 1 {
			first, second = hold, change
		}
		wg.Add(2)
		go first()
		waitForLockWaiters(t, f.owner, 1)
		go second()
		waitForLockWaiters(t, f.owner, 2)
		f.must(t, lock.Commit(ctx))
		wg.Wait()
		var so sales.SoldOut
		switch {
		case (changeErr == nil) == (holdErr == nil):
			t.Fatalf("round %d: change %v, hold %v: want exactly one winner", round, changeErr, holdErr)
		case changeErr != nil && !errors.As(changeErr, &so), holdErr != nil && !errors.As(holdErr, &so):
			t.Fatalf("round %d: change %v, hold %v: the loser should be sold_out", round, changeErr, holdErr)
		}
		if n := f.used(t, vipQ); n != 1 {
			t.Fatalf("round %d: VIP used %d, want 1", round, n)
		}
		// Free the seat for the next round.
		f.exec(t, `delete from holds`)
		f.exec(t, `update tickets set ticket_type_id = $1 where id = $2`, f.general, tickets[0])
	}
}

func TestChangeTypeRefusals(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	vip := f.ticketType(t, f.org, f.event, "VIP", 2500, false)
	vipQ := f.quota(t, f.org, f.event, "VIP", 1, vip)
	hidden := f.ticketType(t, f.org, f.event, "Crew", 2500, true)
	f.quota(t, f.org, f.event, "Crew", 5, hidden)
	balcony := f.ticketType(t, f.org, f.event, "Balcony", 4000, false)
	f.quota(t, f.org, f.event, "Balcony", 5, balcony)
	taxed := f.ticketType(t, f.org, f.event, "Taxed", 2500, false)
	f.exec(t, `update ticket_types set tax_rate_bp = 825 where id = $1`, taxed)
	inclusive := f.ticketType(t, f.org, f.event, "Inclusive", 2500, false)
	f.exec(t, `update ticket_types set prices_include_tax = true where id = $1`, inclusive)
	f.quota(t, f.org, f.event, "Taxed", 5, taxed, inclusive)
	loose := f.ticketType(t, f.org, f.event, "Loose", 2500, false)
	other := f.id(t, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, 'other', 'Other', 'UTC', now() + interval '2 days', now() + interval '3 days', 'live') returning id`, f.org)
	elsewhere := f.ticketType(t, f.org, other, "GA", 2500, false)
	f.quota(t, f.org, other, "GA", 5, elsewhere)
	order, _, tickets := f.paidOrder(t, s, fake, 3, 1)
	tk := tickets[0]

	for label, c := range map[string]struct {
		to          uuid.UUID
		code, field string
	}{
		"dearer":       {balcony, "price_differs", ""},
		"taxed":        {taxed, "price_differs", ""},
		"tax included": {inclusive, "price_differs", ""},
		"same":         {f.general, "same_type", "ticket_type_id"},
		"other event":  {elsewhere, "unknown_ticket_type", "ticket_type_id"},
		"no such type": {uuid.New(), "unknown_ticket_type", "ticket_type_id"},
		"no quota":     {loose, "no_quota", "ticket_type_id"},
	} {
		_, err := f.changeType(s, order, tk, c.to)
		if got := conflictCode(err) + invalidCode(err); got != c.code || fieldOf(err) != c.field {
			t.Errorf("%s: %v, want %s on %q", label, err, c.code, c.field)
		}
	}
	var c catalogue.ErrConflict
	if _, err := f.changeType(s, order, tk, balcony); !errors.As(err, &c) || !strings.Contains(c.Message, "manual order") {
		t.Errorf("price_differs copy: %v", err)
	}

	f.manual(t, s, comp(f, sales.Item{TicketTypeID: vip, Quantity: 1}))
	_, err := f.changeType(s, order, tk, vip)
	if left := remaining(err); left == nil || left[vip] != 0 || left[hidden] != 5 {
		t.Errorf("full VIP: %v (%v), want sold_out naming Crew too", err, left)
	}
	if f.used(t, vipQ) != 1 {
		t.Fatal("a refused change took a seat")
	}

	f.exec(t, `update tickets set status = 'void', released_at = now() where id = $1`, tickets[1])
	if _, err := f.changeType(s, order, tickets[1], hidden); conflictCode(err) != "ticket_void" || strings.Contains(err.Error(), "name") {
		t.Errorf("void: %v, want ticket_void without rename copy", err)
	}
	if r := f.scan(t, s, f.qrOf(t, tickets[2])); r.Result != "accepted" {
		t.Fatalf("check-in: %q", r.Result)
	}
	if _, err := f.changeType(s, order, tickets[2], hidden); !errors.Is(err, sales.ErrCheckedIn) {
		t.Errorf("checked in: %v, want checked_in", err)
	}
	if _, err := f.changeType(s, uuid.New(), tk, hidden); !errors.Is(err, errNotFound) {
		t.Errorf("no such order: %v", err)
	}
	_, _, theirs := f.paidOrder(t, s, fake, 1, 2)
	if _, err := f.changeType(s, order, theirs[0], hidden); !errors.Is(err, errNotFound) {
		t.Errorf("another order's ticket: %v", err)
	}
	if f.typeOf(t, tk) != f.general {
		t.Fatal("a refused change moved the ticket")
	}
}

func TestChangeTypeComp(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500, taxBP: 825})
	balcony := f.ticketType(t, f.org, f.event, "Balcony", 4000, false)
	f.exec(t, `update ticket_types set prices_include_tax = true where id = $1`, balcony)
	f.quota(t, f.org, f.event, "Balcony", 5, balcony)
	r := f.manual(t, s, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1}))
	tk := f.id(t, `select id from tickets where order_id = $1`, r.ID)
	if _, err := f.changeType(s, r.ID, tk, balcony); err != nil {
		t.Fatalf("comp GA to Balcony: %v", err)
	}
	if f.typeOf(t, tk) != balcony {
		t.Fatal("comp ticket didn't move")
	}
}

// The change stands when the email can't go yet.
func TestChangeTypeEmailThrottled(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	vip := f.ticketType(t, f.org, f.event, "VIP", 2500, false)
	f.quota(t, f.org, f.event, "VIP", 5, vip)
	order, _, tickets := f.paidOrder(t, s, fake, 1, 1)
	f.exec(t, `delete from jobs where kind = 'order_confirmation'`)
	f.exec(t, `update orders set mail_status = 'sent', mail_requested_at = now() where id = $1`, order)

	email, err := f.changeType(s, order, tickets[0], vip)
	if err != nil || email != "too_soon" {
		t.Fatalf("%q %v, want too_soon", email, err)
	}
	if f.typeOf(t, tickets[0]) != vip {
		t.Fatal("the change didn't stand")
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'order_confirmation'`); n != 0 {
		t.Fatalf("%d jobs, want none", n)
	}
}

// set_ticket_type's refusals, behind the locks, map to the same answers.
func TestChangeTypeMapsDefinerErrors(t *testing.T) {
	for msg, want := range map[string]string{"same_type": "same_type", "checked_in": "checked_in", "ticket_void": "ticket_void",
		"price_differs": "price_differs", "unknown_ticket_type": "unknown_ticket_type"} {
		err := sales.TypeErrorForTest(&pgconn.PgError{Code: "22023", Message: msg})
		if got := conflictCode(err) + invalidCode(err); got != want {
			t.Errorf("%s: %v", msg, err)
		}
	}
}
