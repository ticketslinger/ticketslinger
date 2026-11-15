package sales_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/checkin"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/emails"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/reports"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func (f fixture) block(s *sales.Service, order, ticket uuid.UUID, reason string) error {
	return db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
		return sales.BlockTicket(context.Background(), tx, f.org, f.event, order, ticket, reason, audit.Actor{User: f.user})
	})
}

func (f fixture) unblock(s *sales.Service, order, ticket uuid.UUID) error {
	return db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
		return sales.UnblockTicket(context.Background(), tx, f.org, f.event, order, ticket, audit.Actor{User: f.user})
	})
}

func TestBlockTicket(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	order, _, _, tickets := f.linkedOrder(t, s, fake, 2, 1)
	_, _, _, others := f.linkedOrder(t, s, fake, 1, 2)
	audits := func(action string) int {
		return f.count(t, `select count(*) from audit_log where action = $1 and entity_type = 'ticket' and entity_id = $2`, action, tickets[0])
	}

	f.must(t, f.block(s, order, tickets[0], "  Chargeback on the card  "))
	if n := f.count(t, `select count(*) from tickets where id = $1 and blocked_at is not null and blocked_reason = 'Chargeback on the card'`, tickets[0]); n != 1 {
		t.Fatal("block didn't set blocked_at and the trimmed reason")
	}
	if after := f.str(t, `select after::text from audit_log where action = 'ticket.blocked'`); after != `{"reason": "Chargeback on the card"}` {
		t.Fatalf("ticket.blocked audit after: %s", after)
	}
	var d sales.OrderDetailView
	f.must(t, db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) (err error) {
		d, err = sales.OrderDetail(context.Background(), tx, f.event, order)
		return err
	}))
	if dt := d.Tickets[0]; dt.ID != tickets[0] || dt.TicketTypeID != f.general || dt.BlockedAt == nil || dt.BlockedReason == nil ||
		*dt.BlockedReason != "Chargeback on the card" || dt.CheckedInAt != nil || d.Tickets[1].BlockedAt != nil {
		t.Fatalf("order detail: %+v", d.Tickets)
	}
	at := f.str(t, `select blocked_at::text from tickets where id = $1`, tickets[0])
	f.must(t, f.block(s, order, tickets[0], "something else"))
	if audits("ticket.blocked") != 1 || f.str(t, `select blocked_at::text || blocked_reason from tickets where id = $1`, tickets[0]) != at+"Chargeback on the card" {
		t.Fatal("blocking a blocked ticket changed something")
	}

	f.must(t, f.unblock(s, order, tickets[0]))
	if n := f.count(t, `select count(*) from tickets where id = $1 and blocked_at is null and blocked_reason is null`, tickets[0]); n != 1 {
		t.Fatal("unblock left the block in place")
	}
	f.must(t, f.unblock(s, order, tickets[0]))
	if audits("ticket.unblocked") != 1 {
		t.Fatalf("%d ticket.unblocked rows, want 1: unblocking an unblocked ticket wrote one", audits("ticket.unblocked"))
	}
	f.must(t, f.block(s, order, tickets[0], ""))
	if n := f.count(t, `select count(*) from tickets where id = $1 and blocked_at is not null and blocked_reason is null`, tickets[0]); n != 1 {
		t.Fatal("a block with no reason should store none")
	}

	if err := f.block(s, order, others[0], ""); !errors.Is(err, errNotFound) {
		t.Fatalf("another order's ticket: %v, want ErrNotFound", err)
	}
	if err := f.unblock(s, order, uuid.New()); !errors.Is(err, errNotFound) {
		t.Fatalf("no such ticket: %v, want ErrNotFound", err)
	}
	for _, bad := range []string{strings.Repeat("a", 201), "A\x00B", "A\u202eB"} {
		var inv catalogue.ErrInvalid
		if err := f.block(s, order, tickets[1], bad); !errors.As(err, &inv) || inv.Code != "bad_reason" || inv.Field != "reason" ||
			inv.Message != "Keep the reason to 200 characters of plain text." {
			t.Errorf("reason %.10q: %v (field %q), want bad_reason on reason", bad, err, fieldOf(err))
		}
	}
	// The reason is checked before the ticket is looked up.
	if err := f.block(s, order, uuid.New(), strings.Repeat("a", 201)); invalidCode(err) != "bad_reason" {
		t.Fatalf("bad reason on an unknown ticket: %v, want bad_reason first", err)
	}
	f.exec(t, `update tickets set status = 'void', released_at = now() where id = $1`, tickets[1])
	if err := f.block(s, order, tickets[1], ""); conflictCode(err) != "ticket_void" {
		t.Fatalf("block a void ticket: %v, want ticket_void", err)
	}
	if err := f.unblock(s, order, tickets[1]); conflictCode(err) != "ticket_void" {
		t.Fatalf("unblock a void ticket: %v, want ticket_void", err)
	}
}

// Review Focus 2: a blocked ticket's QR is on no buyer path.
func TestBlockedTicketHasNoQRAnywhere(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	order, code, token, tickets := f.linkedOrder(t, s, fake, 2, 1)
	f.must(t, f.block(s, order, tickets[0], "Chargeback"))

	o, err := s.Order(ctx, code, token)
	f.must(t, err)
	if len(o.Tickets) != 2 || o.Tickets[0].ID != tickets[0] || o.Tickets[0].QR != "" || !o.Tickets[0].Blocked ||
		o.Tickets[1].QR == "" || o.Tickets[1].Blocked {
		t.Fatalf("buyer's page tickets: %+v, want the blocked one without a QR", o.Tickets)
	}
	pdf, _, err := s.OrderPDF(ctx, code, token)
	f.must(t, err)
	if n := len(regexp.MustCompile(`/Type\s*/Page\b`).FindAll(pdf, -1)); n != 1 {
		t.Fatalf("%d PDF pages, want 1: the blocked ticket has none", n)
	}

	var sent emails.TicketsData
	sales.SetTicketsEmailForTest(s, func(d emails.TicketsData) (mailer.Message, error) {
		sent = d
		return emails.Tickets(d)
	})
	msg := f.ticketEmail(t, s, order)
	if len(sent.Tickets) != 1 || strings.Contains(msg.Text+msg.HTML, f.qrOf(t, tickets[0])) {
		t.Fatalf("ticket email: %d tickets, want only the unblocked one", len(sent.Tickets))
	}

	f.must(t, f.block(s, order, tickets[1], ""))
	f.exec(t, `update orders set mail_status = 'sent', mail_requested_at = now() - interval '11 minutes' where id = $1`, order)
	err = db.WithOrganizer(ctx, s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
		return sales.QueueTicketEmailForTest(ctx, tx, order)
	})
	if conflictCode(err) != "no_tickets" {
		t.Fatalf("queue with every ticket blocked: %v, want no_tickets", err)
	}
}

// A block holds the door, not the seat: the ticket is still sold everywhere.
func TestBlockedStillSold(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	order, _, _, tickets := f.linkedOrder(t, s, fake, 2, 1)
	counts := func() (used, sold, doorSold int) {
		f.must(t, db.WithOrganizer(ctx, s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
			r, err := reports.Event(ctx, tx, f.event)
			if err != nil {
				return err
			}
			sold = r.Sold
			_, doorSold, err = checkin.Summary(ctx, tx, f.event)
			return err
		}))
		return f.quotaUsed(t), sold, doorSold
	}
	u0, s0, d0 := counts()
	f.must(t, f.block(s, order, tickets[0], ""))
	if u, sold, d := counts(); u != u0 || sold != s0 || d != d0 || sold != 2 {
		t.Fatalf("after a block: quota used %d (was %d), report sold %d (was %d), door sold %d (was %d)", u, u0, sold, s0, d, d0)
	}
}
