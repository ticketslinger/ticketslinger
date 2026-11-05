package sales_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// alertsOn turns the owner's alerts on; the admin has them off and a
// check-in member has them forced on, which must change nothing.
func (f fixture) alertsOn(t *testing.T) {
	t.Helper()
	f.exec(t, `update memberships set order_alerts = true where user_id = $1`, f.user)
	f.exec(t, `insert into users(email, name) values ('admin@example.com', 'Admin'), ('door@example.com', 'Door')`)
	f.exec(t, `insert into memberships(organizer_id, user_id, role, order_alerts)
		select $1, id, case email when 'admin@example.com' then 'admin' else 'checkin' end, email = 'door@example.com' from users
		where email in ('admin@example.com', 'door@example.com')`, f.org)
}

func (f fixture) alertJobs(t *testing.T) (out []map[string]any) {
	t.Helper()
	rows, err := f.owner.Query(context.Background(), `select payload from jobs where kind = 'order_alert' order by id`)
	f.must(t, err)
	defer rows.Close()
	for rows.Next() {
		var b []byte
		f.must(t, rows.Scan(&b))
		var m map[string]any
		f.must(t, json.Unmarshal(b, &m))
		out = append(out, m)
	}
	return out
}

func TestAlertsOnOnlineOrders(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 20, price: 2600})
	f.alertsOn(t)
	f.exec(t, `update events set fee_mode = 'absorb' where id = $1`, f.event) // no fee on top: 2 x $26 is $52
	c, pi := paying(t, s, f, fake, sales.Details{Email: "buyer@example.com", Attendees: []string{"Ada", "Bob"}}, sales.Item{TicketTypeID: f.general, Quantity: 2})
	f.must(t, s.HandleStripeEvent(context.Background(), succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	order, code, _ := f.orderOf(t, c.ID)
	js := f.alertJobs(t)
	if len(js) != 1 || js[0]["user_id"] != f.user.String() || js[0]["nth"] != float64(1) || js[0]["order_id"] != order.String() {
		t.Fatalf("jobs %v, want one for the owner, nth 1", js)
	}
	if strings.Contains(js[0]["user_id"].(string), "@") || len(js[0]) != 4 {
		t.Fatalf("payload %v carries more than ids and a count", js[0])
	}
	var payload []byte
	f.must(t, f.owner.QueryRow(context.Background(), `select payload from jobs where kind = 'order_alert'`).Scan(&payload))
	m := &mailer.Memory{}
	f.must(t, s.OrderAlert(m)(context.Background(), payload))
	if len(m.Sent) != 1 {
		t.Fatalf("%d messages, want 1", len(m.Sent))
	}
	got := m.Sent[0]
	if got.To != "a@example.com" || got.Subject != "New order "+code+" for Gala" {
		t.Errorf("to %q subject %q", got.To, got.Subject)
	}
	for _, want := range []string{"2 tickets for Gala: 2 × General. Total $52.00.", "https://app.example/o/acme/events/gala/orders/" + order.String()} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, got.Text)
		}
	}
	for _, no := range []string{"buyer@example.com", "Ada", "Bob"} {
		if strings.Contains(got.Text+got.Subject, no) {
			t.Errorf("alert leaks %q", no)
		}
	}
}

// Free online orders alert (P7-D16); comp and manual orders don't (P7-D6).
func TestAlertsFreeYesCompNo(t *testing.T) {
	s, _ := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 20, price: 0})
	f.alertsOn(t)
	ctx := context.Background()
	for _, kind := range []string{"comp", "manual"} {
		in := comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1})
		in.Kind = kind
		if kind == "manual" {
			in.Method, in.AmountCents = "cash", 0
		}
		f.manual(t, s, in)
	}
	if n := len(f.alertJobs(t)); n != 0 {
		t.Fatalf("%d alerts for comp and manual orders, want 0", n)
	}
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	_, err := s.Pay(ctx, c.Token, sales.Details{Email: "free@example.com"})
	f.must(t, err)
	order, code, _ := f.orderOf(t, c.ID)
	js := f.alertJobs(t)
	if len(js) != 1 || js[0]["order_id"] != order.String() {
		t.Fatalf("jobs %v, want one for the free order", js)
	}
	m := &mailer.Memory{}
	var payload []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'order_alert'`).Scan(&payload))
	f.must(t, s.OrderAlert(m)(ctx, payload))
	if len(m.Sent) != 1 || !strings.Contains(m.Sent[0].Text, "1 ticket for Gala: 1 × General. Total Free.") || m.Sent[0].Subject != "New order "+code+" for Gala" {
		t.Fatalf("sent %+v", m.Sent)
	}
}

func TestAlertCopy(t *testing.T) {
	a := sales.AlertMail{Event: "Gala", Organizer: "Acme", Code: "K7Q2", Lines: []string{"2 × General", "1 × VIP"}, Tickets: 3, TotalCents: 7800,
		OrderURL: "https://app.example/o/acme/events/gala/orders/o1", OrdersURL: "https://app.example/o/acme/events/gala/orders", TeamURL: "https://app.example/o/acme/team"}
	const body = "3 tickets for Gala: 2 × General, 1 × VIP. Total $78.00.\n\nSee the order: https://app.example/o/acme/events/gala/orders/o1\n\n"
	const tail = "You get these because you turned on order emails for Acme. Turn them off on the Team page: https://app.example/o/acme/team"
	subject, text := sales.OrderAlertEmail(a)
	if subject != "New order K7Q2 for Gala" || text != body+tail {
		t.Errorf("subject %q text %q", subject, text)
	}
	a.Last, a.TotalCents, a.Tickets, a.Lines = true, 0, 1, []string{"1 × General"}
	_, text = sales.OrderAlertEmail(a)
	want := "1 ticket for Gala: 1 × General. Total Free.\n\nSee the order: https://app.example/o/acme/events/gala/orders/o1\n\n" +
		"That's the last alert today: we stop at 50 a day so a big on-sale doesn't bury your inbox. Every order is on the Orders tab: https://app.example/o/acme/events/gala/orders\n\n" + tail
	if text != want {
		t.Errorf("last text %q", text)
	}
}

// Review Focus 4: the 50th alert of a UTC day goes with the last-today line, the 51st isn't sent.
func TestAlertCapThroughConfirm(t *testing.T) {
	s, _ := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 20, price: 0})
	f.alertsOn(t)
	ctx := context.Background()
	f.exec(t, `insert into order_alert_counts(organizer_id, user_id, day, sent) values ($1, $2, (now() at time zone 'utc')::date, 49)
		on conflict (organizer_id, user_id, day) do update set sent = 49`, f.org, f.user)
	// Yesterday's row (UTC) is cleared, not counted.
	f.exec(t, `insert into order_alert_counts(organizer_id, user_id, day, sent) values ($1, $2, (now() at time zone 'utc')::date - 1, 50)`, f.org, f.user)
	order := func(email string) {
		c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
		_, err := s.Pay(ctx, c.Token, sales.Details{Email: email})
		f.must(t, err)
	}
	order("one@example.com")
	js := f.alertJobs(t)
	if len(js) != 1 || js[0]["nth"] != float64(50) {
		t.Fatalf("jobs %v, want one with nth 50", js)
	}
	var payload []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'order_alert'`).Scan(&payload))
	m := &mailer.Memory{}
	f.must(t, s.OrderAlert(m)(ctx, payload))
	if len(m.Sent) != 1 || !strings.Contains(m.Sent[0].Text, "That's the last alert today") {
		t.Fatalf("50th: %+v", m.Sent)
	}
	order("two@example.com")
	if n := len(f.alertJobs(t)); n != 1 {
		t.Fatalf("%d jobs after the 51st order, want still 1", n)
	}
	if n := f.count(t, `select count(*) from order_alert_counts where day < (now() at time zone 'utc')::date`); n != 0 {
		t.Fatalf("%d old day rows kept", n)
	}
}

func TestAlertSkipsRemovedMember(t *testing.T) {
	s, _ := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 20, price: 0})
	f.alertsOn(t)
	ctx := context.Background()
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	_, err := s.Pay(ctx, c.Token, sales.Details{Email: "free@example.com"})
	f.must(t, err)
	var payload []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'order_alert'`).Scan(&payload))
	f.exec(t, `update memberships set order_alerts = false where user_id = $1`, f.user)
	m := &mailer.Memory{}
	if err := s.OrderAlert(m)(ctx, payload); err != nil || len(m.Sent) != 0 {
		t.Fatalf("opted out: err %v, sent %d", err, len(m.Sent))
	}
	f.exec(t, `update memberships set order_alerts = true where user_id = $1`, f.user)
	f.exec(t, `delete from memberships where user_id = $1`, f.user)
	if err := s.OrderAlert(m)(ctx, payload); err != nil || len(m.Sent) != 0 {
		t.Fatalf("removed: err %v, sent %d", err, len(m.Sent))
	}
}
