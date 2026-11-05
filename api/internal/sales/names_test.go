package sales_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"  Ada ":                 "Ada",
		"":                       "",
		"   ":                    "",
		strings.Repeat("é", 120): strings.Repeat("é", 120),
		"Zoë O'Brien-Núñez":      "Zoë O'Brien-Núñez",
	} {
		got, err := sales.CleanName(in)
		if err != nil || got != want {
			t.Errorf("CleanName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for name, in := range map[string]string{
		"121 characters":       strings.Repeat("é", 121),
		"bidi override (Cf)":   "A\u202eda",
		"newline (Cc)":         "A\nda",
		"zero-width space":     "A\u200bda",
		"line separator":       "A\u2028da",
		"paragraph separator":  "A\u2029da",
		"NUL":                  "A\x00da",
		"invalid UTF-8":        "\xff",
		"private use (Co)":     "A\ue000da",
		"word joiner (Cf)":     "A\u2060da",
		"soft hyphen (Cf)":     "A\u00adda",
		"invalid in the midst": "Ad\xc3a",
	} {
		if _, err := sales.CleanName(in); invalidCode(err) != "bad_attendee" {
			t.Errorf("%s: %v, want bad_attendee", name, err)
		}
	}
}

func TestCheckoutRefusesBadNames(t *testing.T) {
	s, f, _ := payService(t, seedOpts{quota: 5, price: 2500})
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	for _, bad := range []string{"A\u202eda", "A\u2028da", strings.Repeat("a", 121)} {
		_, err := s.Pay(context.Background(), c.Token, sales.Details{Email: "buyer@example.com", Attendees: []string{bad}})
		if invalidCode(err) != "bad_attendee" || fieldOf(err) != "attendees" {
			t.Errorf("%q: %v (field %q), want bad_attendee on attendees", bad, err, fieldOf(err))
		}
	}
	if n := f.count(t, `select count(*) from orders`); n != 0 {
		t.Fatalf("%d orders after refused names, want none", n)
	}
}

// linkedOrder is a paid order of qty General tickets, as Stripe event n,
// with the code and token of its link and its tickets in id order.
func (f fixture) linkedOrder(t *testing.T, s *sales.Service, fake *payments.Fake, qty, n int) (order uuid.UUID, code, token string, tickets []uuid.UUID) {
	t.Helper()
	order, _, tickets = f.paidOrder(t, s, fake, qty, n)
	return order, f.str(t, `select code from orders where id = $1`, order), sales.OrderToken(s.SecretKey, order), tickets
}

func TestRenameByLinkMapsAnswers(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	_, code, token, tickets := f.linkedOrder(t, s, fake, 2, 1)
	_, _, _, others := f.linkedOrder(t, s, fake, 1, 2)
	tk := tickets[0]
	name := func(id uuid.UUID) string { return f.str(t, `select attendee_name from tickets where id = $1`, id) }
	changes := func() int { return f.count(t, `select name_changes from tickets where id = $1`, tk) }

	for label, try := range map[string]struct {
		code, token string
		ticket      uuid.UUID
	}{
		"wrong token":          {code, "wrong", tk},
		"empty token":          {code, "", tk},
		"unknown code":         {"TS-00000", token, tk},
		"not a code":           {"nope", token, tk},
		"another order's":      {code, token, others[0]},
		"no such ticket":       {code, token, uuid.New()},
		"another order's link": {code, sales.OrderToken(s.SecretKey, uuid.New()), tk},
	} {
		if err := s.RenameByLink(ctx, try.code, try.token, try.ticket, "Ada"); !errors.Is(err, errNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", label, err)
		}
	}

	f.must(t, s.RenameByLink(ctx, strings.ToLower(code), token, tk, "  Ada  "))
	if name(tk) != "Ada" || changes() != 1 {
		t.Fatalf("after a rename: %q, %d changes; want Ada, 1", name(tk), changes())
	}
	f.must(t, s.RenameByLink(ctx, code, token, tk, "Ada")) // unchanged: no change counted
	if changes() != 1 || f.count(t, `select count(*) from audit_log where action = 'ticket.renamed'`) != 1 {
		t.Fatalf("an unchanged name counted as a change")
	}
	for _, bad := range []string{"A\u202eda", "A\u2028da", "A\x00da", strings.Repeat("a", 121)} {
		err := s.RenameByLink(ctx, code, token, tk, bad)
		if invalidCode(err) != "bad_attendee" || fieldOf(err) != "attendee_name" {
			t.Errorf("%q: %v (field %q), want bad_attendee on attendee_name", bad, err, fieldOf(err))
		}
	}

	f.exec(t, `update tickets set name_changes = 5 where id = $1`, tk)
	if err := s.RenameByLink(ctx, code, token, tk, "Bob"); conflictCode(err) != "name_change_limit" {
		t.Fatalf("sixth change: %v, want name_change_limit", err)
	}
	f.must(t, s.RenameByLink(ctx, code, token, tk, "Ada")) // the same name is no change, even at the limit

	f.exec(t, `update events set names_locked_at = now() - interval '1 minute' where id = $1`, f.event)
	if err := s.RenameByLink(ctx, code, token, tickets[1], "Cy"); conflictCode(err) != "names_locked" {
		t.Fatalf("after the lock: %v, want names_locked", err)
	}
	if name(tickets[1]) != "" {
		t.Fatalf("a locked rename wrote %q", name(tickets[1]))
	}
}

// The function's own check is a backstop; if it ever refuses a name Go let
// through, or Postgres refuses a NUL, the buyer still gets a 422.
func TestRenameByLinkMapsTheDatabaseRefusals(t *testing.T) {
	for _, pg := range []*pgconn.PgError{{Code: "22023", Message: "bad_attendee"}, {Code: "22021", Message: "invalid byte sequence for encoding \"UTF8\": 0x00"}} {
		err := sales.RenameErrorForTest(pg)
		if invalidCode(err) != "bad_attendee" || fieldOf(err) != "attendee_name" {
			t.Errorf("%s: %v, want bad_attendee on attendee_name", pg.Code, err)
		}
	}
	other := &pgconn.PgError{Code: "22023", Message: "something else"}
	if err := sales.RenameErrorForTest(other); !errors.Is(err, other) {
		t.Errorf("another 22023: %v, want it passed through", err)
	}
}

func TestRenameTicket(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	order, _, _, tickets := f.linkedOrder(t, s, fake, 2, 1)
	other, _, _, others := f.linkedOrder(t, s, fake, 1, 2)
	rename := func(order, ticket uuid.UUID, name string) error {
		return db.WithOrganizer(ctx, s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
			return sales.RenameTicket(ctx, tx, f.org, f.event, order, ticket, name, audit.Actor{User: f.user})
		})
	}
	renamed := func() int { return f.count(t, `select count(*) from audit_log where action = 'ticket.renamed'`) }

	f.must(t, rename(order, tickets[0], " Ada "))
	var actor uuid.UUID
	var before, after, entity string
	f.must(t, f.owner.QueryRow(ctx, `select actor_user_id, entity_id::text, before::text, after::text from audit_log
		where action = 'ticket.renamed' and entity_type = 'ticket'`).Scan(&actor, &entity, &before, &after))
	if actor != f.user || entity != tickets[0].String() || before != `{"attendee_name": ""}` || after != `{"attendee_name": "Ada"}` {
		t.Fatalf("audit row: %s %s %s %s", actor, entity, before, after)
	}
	if n := f.count(t, `select name_changes from tickets where id = $1`, tickets[0]); n != 0 {
		t.Fatalf("an organizer's rename counted against the buyer's five: %d", n)
	}

	f.must(t, rename(order, tickets[0], "Ada"))
	if renamed() != 1 {
		t.Fatal("the same name wrote an audit row")
	}
	if err := rename(order, others[0], "Bob"); !errors.Is(err, errNotFound) {
		t.Fatalf("another order's ticket: %v, want ErrNotFound", err)
	}
	if err := rename(other, tickets[1], "Bob"); !errors.Is(err, errNotFound) {
		t.Fatalf("this ticket under another order: %v, want ErrNotFound", err)
	}
	if err := rename(order, uuid.New(), "Bob"); !errors.Is(err, errNotFound) {
		t.Fatalf("no such ticket: %v, want ErrNotFound", err)
	}
	if err := rename(order, tickets[1], "A\u202eda"); invalidCode(err) != "bad_attendee" || fieldOf(err) != "attendee_name" {
		t.Fatalf("bad name: %v (field %q), want bad_attendee on attendee_name", err, fieldOf(err))
	}
	f.exec(t, `update tickets set status = 'void' where id = $1`, tickets[1])
	if err := rename(order, tickets[1], "Bob"); conflictCode(err) != "ticket_void" {
		t.Fatalf("void ticket: %v, want ticket_void", err)
	}
	if renamed() != 1 || f.str(t, `select attendee_name from tickets where id = $1`, tickets[1]) != "" {
		t.Fatal("a refused rename wrote something")
	}
	// After the event starts the buyer is locked out; the organizer isn't.
	f.exec(t, `update events set names_locked_at = now() - interval '1 minute' where id = $1`, f.event)
	f.must(t, rename(order, tickets[0], ""))
	if f.str(t, `select attendee_name from tickets where id = $1`, tickets[0]) != "" || renamed() != 2 {
		t.Fatal("clearing the name didn't take")
	}
}
