package sales_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func comp(f fixture, items ...sales.Item) sales.ManualOrderInput {
	return sales.ManualOrderInput{Event: f.event, Kind: "comp", Email: "guest@example.com", Items: items, SendTickets: true}
}

func (f fixture) manual(t *testing.T, s *sales.Service, in sales.ManualOrderInput) sales.ManualOrderResult {
	t.Helper()
	r, err := s.CreateManualOrder(context.Background(), f.org, f.user, in)
	f.must(t, err)
	return r
}

func (f fixture) charges(t *testing.T, order uuid.UUID, col string) (out []int64) {
	t.Helper()
	rows, _ := f.owner.Query(context.Background(), `select `+col+` from tickets where order_id = $1 order by id`, order)
	out, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	f.must(t, err)
	return out
}

func TestCompOrder(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500, taxBP: 825})
	in := comp(f, sales.Item{TicketTypeID: f.general, Quantity: 2})
	in.BuyerName, in.Attendees = "Dana Ruiz", []string{"Ada"}
	r := f.manual(t, s, in)
	var status, source, code string
	var total, fee, tax int64
	var paid, hashed, noCheckout, byUser bool
	f.must(t, f.owner.QueryRow(context.Background(), `select status, source, code, total_cents, fee_cents, tax_cents, paid_at is not null,
			magic_token_hash is not null, checkout_id is null, created_by = $2 from orders where id = $1`, r.ID, f.user).
		Scan(&status, &source, &code, &total, &fee, &tax, &paid, &hashed, &noCheckout, &byUser))
	if status != "paid" || source != "comp" || code != r.Code || total != 0 || fee != 0 || tax != 0 || !paid || !hashed || !noCheckout || !byUser {
		t.Fatalf("order %s %s %s %d/%d/%d paid=%v hash=%v nocheckout=%v by=%v", status, source, code, total, fee, tax, paid, hashed, noCheckout, byUser)
	}
	if p, c := f.charges(t, r.ID, "price_cents"), f.charges(t, r.ID, "charge_cents"); len(p) != 2 || p[0]+p[1]+c[0]+c[1] != 0 {
		t.Fatalf("tickets price %v charge %v", p, c)
	}
	if n := f.count(t, `select count(*) from tickets where order_id = $1 and attendee_name = 'Ada'`, r.ID); n != 1 {
		t.Fatalf("%d tickets named Ada", n)
	}
	if n := f.count(t, `select count(*) from payments where order_id = $1`, r.ID); n != 0 {
		t.Fatalf("%d payments", n)
	}
	f.wantMail(t, r.ID, "queued", "", 0)
	if n := f.count(t, `select count(*) from jobs where kind = $1 and payload->>'order_id' = $2`, sales.OrderConfirmationJob, r.ID.String()); n != 1 {
		t.Fatalf("%d ticket emails queued", n)
	}
	var after string
	f.must(t, f.owner.QueryRow(context.Background(), `select after::text from audit_log where action = 'order.created_manual'
		and entity_type = 'order' and entity_id = $1 and actor_user_id = $2 and not actor_system`, r.ID, f.user).Scan(&after))
	if strings.Contains(after, "guest@") || strings.Contains(after, "Dana") || strings.Contains(after, "Ada") ||
		!strings.Contains(after, `"kind": "comp"`) || !strings.Contains(after, `"send_tickets": true`) || !strings.Contains(after, f.general.String()) {
		t.Fatalf("audit after %s", after)
	}
}

func TestManualOrderSplitsTheAmount(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500, taxBP: 825})
	vip := f.addVIP(t, 1000, false)
	r := f.manual(t, s, sales.ManualOrderInput{Event: f.event, Kind: "manual", Method: "cash", AmountCents: 1001, Email: "cash@example.com",
		Items: []sales.Item{{TicketTypeID: f.general, Quantity: 2}, {TicketTypeID: vip, Quantity: 1}}})
	var total, sub, fee int64
	var method string
	f.must(t, f.owner.QueryRow(context.Background(), `select total_cents, subtotal_cents, fee_cents, manual_method from orders where id = $1`, r.ID).
		Scan(&total, &sub, &fee, &method))
	if total != 1001 || sub != 6000 || fee != 0 || method != "cash" {
		t.Fatalf("order total %d subtotal %d fee %d method %s", total, sub, fee, method)
	}
	// Tickets are cut in item order, and their ids are v7, so id order is that order.
	if c, p, fe := f.charges(t, r.ID, "charge_cents"), f.charges(t, r.ID, "price_cents"), f.charges(t, r.ID, "fee_cents"); !equal(c, 417, 417, 167) ||
		!equal(p, 2500, 2500, 1000) || !equal(fe, 0, 0, 0) {
		t.Fatalf("charges %v prices %v fees %v", c, p, fe)
	}
}

func equal(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestManualOrderIgnoresSaleRules(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	vip := f.addVIP(t, 5000, true)
	f.manual(t, s, comp(f, sales.Item{TicketTypeID: vip, Quantity: 1}))
	f.exec(t, `update ticket_types set sales_start = now() - interval '2 days', sales_end = now() - interval '1 day' where id = $1`, f.general)
	f.manual(t, s, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.exec(t, `update events set status = 'draft' where id = $1`, f.event)
	f.manual(t, s, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.exec(t, `update events set status = 'live', starts_at = now() - interval '2 days', ends_at = now() - interval '1 day' where id = $1`, f.event)
	f.manual(t, s, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1}))
	if n := f.count(t, `select count(*) from orders where source = 'comp' and status = 'paid'`); n != 4 {
		t.Fatalf("%d comp orders, want 4", n)
	}
}

func TestManualOrderRefusals(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 200, price: 2500})
	ctx := context.Background()
	other := f.id(t, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, 'other', 'Other', 'America/New_York', now() + interval '6 days', now() + interval '7 days', 'live') returning id`, f.org)
	otherType := f.ticketType(t, f.org, other, "Elsewhere", 1000, false)
	loose := f.ticketType(t, f.org, f.event, "Loose", 1000, false)
	door := f.id(t, `insert into users(email, name) values ('door@example.com', 'Door') returning id`)
	f.exec(t, `insert into memberships(organizer_id, user_id, role) values ($1, $2, 'checkin')`, f.org, door)
	one := sales.Item{TicketTypeID: f.general, Quantity: 1}
	with := func(mod func(*sales.ManualOrderInput)) sales.ManualOrderInput {
		in := comp(f, one)
		mod(&in)
		return in
	}
	type refusal struct {
		name        string
		member      uuid.UUID
		in          sales.ManualOrderInput
		code, field string
		is          error
	}
	cases := []refusal{
		{name: "no kind", in: with(func(in *sales.ManualOrderInput) { in.Kind = "" }), code: "missing_field", field: "kind"},
		{name: "gift", in: with(func(in *sales.ManualOrderInput) { in.Kind = "gift" }), code: "bad_kind", field: "kind"},
		{name: "checkin member", member: door, in: comp(f, one), is: sales.ErrNotManager},
		{name: "other event's type", in: comp(f, sales.Item{TicketTypeID: otherType, Quantity: 1}), code: "unknown_ticket_type", field: "lines"},
		{name: "no quota", in: comp(f, sales.Item{TicketTypeID: loose, Quantity: 1}), code: "no_quota", field: "lines"},
		{name: "101 tickets", in: comp(f, sales.Item{TicketTypeID: f.general, Quantity: 60}, sales.Item{TicketTypeID: loose, Quantity: 41}), code: "too_many_tickets", field: "lines"},
		{name: "no lines", in: comp(f), code: "no_items", field: "lines"},
		{name: "comp with an amount", in: with(func(in *sales.ManualOrderInput) { in.AmountCents = 100 }), code: "bad_amount", field: "amount_cents"},
		{name: "negative amount", in: with(func(in *sales.ManualOrderInput) { in.Kind, in.Method, in.AmountCents = "manual", "cash", -1 }), code: "bad_amount", field: "amount_cents"},
		{name: "amount past the bound", in: with(func(in *sales.ManualOrderInput) { in.Kind, in.Method, in.AmountCents = "manual", "cash", 1000000001 }), code: "bad_amount", field: "amount_cents"},
		{name: "manual, no method", in: with(func(in *sales.ManualOrderInput) { in.Kind = "manual" }), code: "bad_method", field: "method"},
		{name: "comp with a method", in: with(func(in *sales.ManualOrderInput) { in.Method = "cash" }), code: "bad_method", field: "method"},
		{name: "bad email", in: with(func(in *sales.ManualOrderInput) { in.Email = "ada@" }), code: "bad_email", field: "email"},
		{name: "bad buyer name", in: with(func(in *sales.ManualOrderInput) { in.BuyerName = "A\u202eda" }), code: "bad_buyer_name", field: "buyer_name"},
		{name: "bad attendee", in: with(func(in *sales.ManualOrderInput) { in.Attendees = []string{"A\u202eda"} }), code: "bad_attendee", field: "attendees"},
		{name: "two names, one ticket", in: with(func(in *sales.ManualOrderInput) { in.Attendees = []string{"Ada", "Bo"} }), code: "bad_attendee", field: "attendees"},
		{name: "long note", in: with(func(in *sales.ManualOrderInput) { in.Note = strings.Repeat("n", 2001) }), code: "bad_note", field: "note"},
		{name: "note with a control", in: with(func(in *sales.ManualOrderInput) { in.Note = "a\x07b" }), code: "bad_note", field: "note"},
	}
	try := func(c refusal) {
		t.Helper()
		member := f.user
		if c.member != uuid.Nil {
			member = c.member
		}
		before := f.count(t, `select count(*) from orders`)
		_, err := s.CreateManualOrder(ctx, f.org, member, c.in)
		switch {
		case c.is != nil && !errors.Is(err, c.is):
			t.Errorf("%s: %v, want %v", c.name, err, c.is)
		case c.is == nil && (invalidCode(err)+conflictCode(err) != c.code || fieldOf(err) != c.field):
			t.Errorf("%s: %v (%q field %q), want %s field %q", c.name, err, invalidCode(err)+conflictCode(err), fieldOf(err), c.code, c.field)
		}
		if after := f.count(t, `select count(*) from orders`); after != before {
			t.Errorf("%s: orders went from %d to %d", c.name, before, after)
		}
	}
	for _, c := range cases {
		try(c)
	}
	f.exec(t, `update events set status = 'cancelled' where id = $1`, f.event)
	try(refusal{name: "cancelled event", in: comp(f, one), code: "event_cancelled"})
	f.exec(t, `update events set status = 'live' where id = $1`, f.event)
	f.suspend(t, f.org)
	try(refusal{name: "suspended", in: comp(f, one), is: sales.ErrUnavailable})
}

func TestManualOrderRespectsQuotas(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 0})
	vip := f.addVIP(t, 0, true)
	_, err := s.CreateManualOrder(context.Background(), f.org, f.user, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 3}))
	if left := remaining(err); left == nil || left[f.general] != 2 || left[vip] != 2 || len(left) != 2 {
		t.Fatalf("3 of 2: %v (%v), want sold_out naming General and VIP", err, left)
	}
	f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	if _, err := s.CreateManualOrder(context.Background(), f.org, f.user, comp(f, sales.Item{TicketTypeID: vip, Quantity: 2})); remaining(err) == nil {
		t.Fatalf("2 past an open hold: %v, want sold_out", err)
	}
	f.manual(t, s, comp(f, sales.Item{TicketTypeID: vip, Quantity: 1}))
	if n := f.quotaUsed(t); n != 2 {
		t.Fatalf("quota used %d, want 2", n)
	}
}

// Review focus: an organizer's order and a buyer's checkout race for the
// last seat. Both queue on the quota's lock, in turn each queuing first; the
// lock lets exactly one through, every round. Which one isn't pinned: the
// lock grant order isn't promised.
func TestManualOrderRacesCheckoutForLastSeat(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 0})
	ctx := context.Background()
	general := sales.Item{TicketTypeID: f.general, Quantity: 1}
	for round := range 20 {
		lock, err := f.owner.Begin(ctx)
		f.must(t, err)
		_, err = lock.Exec(ctx, `select 1 from quotas where id = $1 for update`, f.hall)
		f.must(t, err)
		var wg sync.WaitGroup
		var manualErr, holdErr, payErr error
		manual := func() {
			defer wg.Done()
			_, manualErr = s.CreateManualOrder(ctx, f.org, f.user, comp(f, general))
		}
		online := func() {
			defer wg.Done()
			c, err := s.CreateCheckout(ctx, buy(general))
			if holdErr = err; err == nil {
				_, payErr = s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"})
			}
		}
		first, second := manual, online
		if round%2 == 1 {
			first, second = online, manual
		}
		wg.Add(2)
		go first()
		waitForLockWaiters(t, f.owner, 1)
		go second()
		waitForLockWaiters(t, f.owner, 2)
		f.must(t, lock.Commit(ctx))
		wg.Wait()
		bought := holdErr == nil && payErr == nil
		var so sales.SoldOut
		switch {
		case (manualErr == nil) == bought:
			t.Fatalf("round %d: manual %v, hold %v, pay %v: want exactly one winner", round, manualErr, holdErr, payErr)
		case manualErr != nil && !errors.As(manualErr, &so), holdErr != nil && !errors.As(holdErr, &so) || payErr != nil:
			t.Fatalf("round %d: manual %v, hold %v, pay %v: the loser should be sold_out", round, manualErr, holdErr, payErr)
		}
		if n := f.quotaUsed(t); n != 1 {
			t.Fatalf("round %d: quota used %d, want 1", round, n)
		}
		// Free the seat for the next round.
		f.exec(t, `update tickets set status = 'void', released_at = now() where status = 'valid'`)
	}
}

func TestSendTicketsFalse(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	in := comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1})
	in.SendTickets = false
	r := f.manual(t, s, in)
	f.wantMail(t, r.ID, "", "", 0)
	if n := f.count(t, `select count(*) from jobs where kind = $1`, sales.OrderConfirmationJob); n != 0 {
		t.Fatalf("%d ticket emails queued", n)
	}
	asOwner(t, s, f, func(tx pgx.Tx) error {
		_, err := sales.ResendTicketEmail(context.Background(), tx, f.org, f.event, r.ID, audit.Actor{User: f.user})
		return err
	})
	f.wantMail(t, r.ID, "queued", "", 0)
}

func TestFirstNote(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	in := comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1})
	in.Note = "  Sponsor's guest,\r\nseat near the front.  "
	r := f.manual(t, s, in)
	var note uuid.UUID
	var body string
	f.must(t, f.owner.QueryRow(context.Background(), `select id, body from order_notes where order_id = $1 and author = $2 and organizer_id = $3`, r.ID, f.user, f.org).
		Scan(&note, &body))
	if body != "Sponsor's guest,\nseat near the front." {
		t.Fatalf("note body %q", body)
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'order.note_added' and entity_type = 'order' and entity_id = $1
		and actor_user_id = $2 and after = jsonb_build_object('note_id', $3::text)`, r.ID, f.user, note.String()); n != 1 {
		t.Fatalf("%d note audit rows", n)
	}
	// No note, no row and no audit.
	r = f.manual(t, s, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1}))
	if n := f.count(t, `select count(*) from order_notes where order_id = $1`, r.ID) + f.count(t, `select count(*) from audit_log where action = 'order.note_added' and entity_id = $1`, r.ID); n != 0 {
		t.Fatalf("%d note rows for an order with no note", n)
	}
}
