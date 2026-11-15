package sales_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/emails"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func (f fixture) regenerate(s *sales.Service, order uuid.UUID, ids []uuid.UUID) (res sales.RegenerateResult, err error) {
	err = db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		res, err = sales.RegenerateTickets(context.Background(), tx, f.org, f.event, order, ids, audit.Actor{User: f.user})
		return err
	})
	return res, err
}

// Review Focus 3: the old QR is dead at the door, the new one works, the
// buyer's link still opens, and no secret reaches the audit log.
func TestRegenerateOldQRIsUnknown(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	order, code, token, tickets := f.linkedOrder(t, s, fake, 2, 1)
	old, kept := f.qrOf(t, tickets[0]), f.qrOf(t, tickets[1])

	res, err := f.regenerate(s, order, []uuid.UUID{tickets[0]})
	f.must(t, err)
	if !slices.Equal(res.TicketIDs, []uuid.UUID{tickets[0]}) {
		t.Fatalf("regenerated %v, want only %v", res.TicketIDs, tickets[0])
	}
	if f.qrOf(t, tickets[1]) != kept {
		t.Fatal("an unnamed ticket's QR changed")
	}
	if r := f.scan(t, s, old); r.Result != "invalid" {
		t.Fatalf("old QR scans %q, want invalid", r.Result)
	}
	o, err := s.Order(ctx, code, token)
	f.must(t, err)
	fresh := o.Tickets[0].QR
	if o.Tickets[0].ID != tickets[0] || fresh == "" || fresh == old {
		t.Fatalf("buyer's page ticket %+v, want a new QR", o.Tickets[0])
	}
	if r := f.scan(t, s, fresh); r.Result != "accepted" {
		t.Fatalf("new QR scans %q, want accepted", r.Result)
	}

	after := f.str(t, `select after::text from audit_log where action = 'ticket.secret_regenerated' and entity_type = 'order' and entity_id = $1`, order)
	if after != `{"ticket_ids": ["`+tickets[0].String()+`"]}` {
		t.Fatalf("audit after %s", after)
	}
	row := f.str(t, `select coalesce(before::text, '') || after::text from audit_log where action = 'ticket.secret_regenerated'`)
	if regexp.MustCompile(`[0-9a-fA-F]{32}`).MatchString(row) || strings.Contains(row, old) || strings.Contains(row, fresh) {
		t.Fatalf("audit row carries a secret: %s", row)
	}
}

// A confirmation job that hasn't run yet reads the tickets when it does, so
// it carries the new code and no second one is queued.
func TestRegenerateWhileEmailQueued(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	order, _, _, tickets := f.linkedOrder(t, s, fake, 1, 1)
	jobs := func() int {
		return f.count(t, `select count(*) from jobs where kind = 'order_confirmation' and payload->>'order_id' = $1`, order.String())
	}
	if jobs() != 1 {
		t.Fatalf("%d confirmation jobs before, want 1", jobs())
	}
	old := f.qrOf(t, tickets[0])

	res, err := f.regenerate(s, order, nil)
	f.must(t, err)
	if res.Email != "on_its_way" || jobs() != 1 {
		t.Fatalf("email %q with %d jobs, want on_its_way with 1", res.Email, jobs())
	}
	var sent emails.TicketsData
	sales.SetTicketsEmailForTest(s, func(d emails.TicketsData) (mailer.Message, error) {
		sent = d
		return emails.Tickets(d)
	})
	f.exec(t, `update jobs set run_at = now() where kind = 'order_confirmation'`)
	runOne(t, emailWorker(s, &mailer.Memory{}))
	if len(sent.Tickets) != 1 || sent.Tickets[0].QR != f.qrOf(t, tickets[0]) || sent.Tickets[0].QR == old {
		t.Fatalf("emailed QR %+v, want the new secret", sent.Tickets)
	}
}

// The ten-minute throttle on resends doesn't apply: the buyer needs the new code.
func TestRegenerateSkipsThrottle(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	order, _, _, _ := f.linkedOrder(t, s, fake, 1, 1)
	f.exec(t, `delete from jobs where kind = 'order_confirmation'`)
	f.exec(t, `update orders set mail_status = 'sent', mail_requested_at = now() where id = $1`, order)

	res, err := f.regenerate(s, order, nil)
	f.must(t, err)
	if res.Email != "queued" {
		t.Fatalf("email %q, want queued", res.Email)
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'order_confirmation' and payload->>'order_id' = $1`, order.String()); n != 1 {
		t.Fatalf("%d confirmation jobs, want 1", n)
	}

	// Every ticket on hold: the codes still change, and nothing is sent.
	other, _, _, tickets := f.linkedOrder(t, s, fake, 1, 2)
	f.must(t, f.block(s, other, tickets[0], ""))
	f.exec(t, `delete from jobs where kind = 'order_confirmation'`)
	if res, err := f.regenerate(s, other, nil); err != nil || res.Email != "no_tickets" {
		t.Fatalf("all blocked: %+v %v, want no_tickets", res, err)
	}
}

func TestRegenerateSkipsCheckedIn(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	order, _, _, tickets := f.linkedOrder(t, s, fake, 3, 1)
	in := f.qrOf(t, tickets[0])
	if r := f.scan(t, s, in); r.Result != "accepted" {
		t.Fatalf("check-in: %q", r.Result)
	}
	f.exec(t, `update tickets set status = 'void', released_at = now() where id = $1`, tickets[2])
	void := f.qrOf(t, tickets[2])
	valid := f.qrOf(t, tickets[1])

	res, err := f.regenerate(s, order, nil)
	f.must(t, err)
	if !slices.Equal(res.TicketIDs, []uuid.UUID{tickets[1]}) || f.qrOf(t, tickets[1]) == valid {
		t.Fatalf("regenerated %v, want only the valid one, changed", res.TicketIDs)
	}
	if f.qrOf(t, tickets[0]) != in || f.qrOf(t, tickets[2]) != void {
		t.Fatal("a checked-in or void ticket's QR changed")
	}
	if _, err := f.regenerate(s, order, []uuid.UUID{tickets[0]}); !errors.Is(err, sales.ErrCheckedIn) {
		t.Fatalf("named checked-in ticket: %v, want ErrCheckedIn", err)
	}
	if r := f.scan(t, s, f.qrOf(t, tickets[1])); r.Result != "accepted" {
		t.Fatalf("check-in: %q", r.Result)
	}
	if _, err := f.regenerate(s, order, nil); conflictCode(err) != "not_regenerable" {
		t.Fatalf("all checked in or void: %v, want not_regenerable", err)
	}
}

func TestRegenerateRefusals(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	order, _, _, tickets := f.linkedOrder(t, s, fake, 2, 1)
	_, _, _, others := f.linkedOrder(t, s, fake, 1, 2)
	f.exec(t, `update tickets set status = 'void', released_at = now() where id = $1`, tickets[1])
	before := f.qrOf(t, tickets[0])

	for label, ids := range map[string][]uuid.UUID{
		"void":          {tickets[0], tickets[1]},
		"other order's": {tickets[0], others[0]},
		"empty":         {},
	} {
		if _, err := f.regenerate(s, order, ids); invalidCode(err) != "bad_tickets" || fieldOf(err) != "ticket_ids" {
			t.Errorf("%s: %v, want bad_tickets on ticket_ids", label, err)
		}
	}
	many := make([]uuid.UUID, 101)
	for i := range many {
		many[i] = uuid.New()
	}
	if _, err := f.regenerate(s, order, many); invalidCode(err) != "too_many" || strings.Contains(err.Error(), "refund") {
		t.Fatalf("101 tickets: %v, want too_many without refund copy", err)
	}
	if _, err := f.regenerate(s, uuid.New(), nil); !errors.Is(err, errNotFound) {
		t.Fatalf("no such order: %v, want ErrNotFound", err)
	}
	if f.qrOf(t, tickets[0]) != before {
		t.Fatal("a refused regenerate changed a QR")
	}
}

// set_ticket_secret's own refusals, behind the locks: never ticket_void's
// rename copy, and bad_secret stays an error that says nothing.
func TestRegenerateMapsDefinerErrors(t *testing.T) {
	if err := sales.SecretErrorForTest(&pgconn.PgError{Code: "22023", Message: "checked_in"}); !errors.Is(err, sales.ErrCheckedIn) {
		t.Errorf("checked_in: %v", err)
	}
	var c catalogue.ErrConflict
	if err := sales.SecretErrorForTest(&pgconn.PgError{Code: "22023", Message: "ticket_void"}); !errors.As(err, &c) || c.Code != "ticket_void" || strings.Contains(c.Message, "name") {
		t.Errorf("ticket_void: %v", err)
	}
	if err := sales.SecretErrorForTest(&pgconn.PgError{Code: "22023", Message: "bad_secret"}); conflictCode(err) != "" || invalidCode(err) != "" {
		t.Errorf("bad_secret: %v, want a plain error", err)
	}
}
