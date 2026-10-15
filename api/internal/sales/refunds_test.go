package sales_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// latePaid is a payment that lands after its hold ran out and someone else
// took the seat: the order is cancelled and a refund requested.
func latePaid(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake, email string) (sales.Checkout, uuid.UUID, plugin.PaymentIntent) {
	t.Helper()
	c, pi := paying(t, s, f, fake, sales.Details{Email: email}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	expire(t, f, c.ID)
	f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.must(t, s.HandleStripeEvent(context.Background(), succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)))
	order, _, _ := f.orderOf(t, c.ID)
	return c, order, pi
}

// paidOrder is a paid order of qty General tickets, as Stripe event n; its
// tickets in id order.
func (f fixture) paidOrder(t *testing.T, s *sales.Service, fake *payments.Fake, qty, n int) (order uuid.UUID, pi string, tickets []uuid.UUID) {
	t.Helper()
	c, intent := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: qty})
	f.must(t, s.HandleStripeEvent(context.Background(), succeeded(intent.ID, "acct_fake1", c.Quote.TotalCents, n)))
	order, _, _ = f.orderOf(t, c.ID)
	rows, err := f.owner.Query(context.Background(), `select id from tickets where order_id = $1 order by id`, order)
	f.must(t, err)
	tickets, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	f.must(t, err)
	return order, intent.ID, tickets
}

// refundWorker runs refunds and sends their emails to m.
func refundWorker(s *sales.Service, m *mailer.Memory) *jobs.Worker {
	return &jobs.Worker{Pool: s.Pool,
		Handlers: map[string]jobs.Handler{sales.RefundJob: sales.Refund(s), sales.OrderConfirmationJob: s.OrderConfirmation(m),
			"send_email": jobs.SendEmail(m)},
		Parked: map[string]func(context.Context, []byte, string){sales.RefundJob: sales.RefundParked(s)}}
}

// drain runs every due job until none is left, mailing to m. A failed job
// backs off, so it isn't run again here.
func drain(t *testing.T, s *sales.Service, m *mailer.Memory) {
	t.Helper()
	w := refundWorker(s, m)
	for {
		ran, err := w.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

func refundOf(t *testing.T, f fixture, order uuid.UUID) (id uuid.UUID, payload []byte) {
	t.Helper()
	id = f.id(t, `select r.id from refunds r join payments p on p.id = r.payment_id where p.order_id = $1`, order)
	f.must(t, f.owner.QueryRow(context.Background(), `select payload from jobs where kind = 'refund' and payload->>'refund_id' = $1`, id.String()).Scan(&payload))
	return id, payload
}

func TestLatePaymentRefundsThroughARefundRow(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	c, order, pi := latePaid(t, s, f, fake, "late@example.com")
	var refund uuid.UUID
	var amount, fee int64
	var reason, status string
	f.must(t, f.owner.QueryRow(ctx, `select r.id, r.amount_cents, r.fee_cents, r.reason, r.status from refunds r
		join payments p on p.id = r.payment_id where p.order_id = $1`, order).Scan(&refund, &amount, &fee, &reason, &status))
	if amount != c.Quote.TotalCents || fee != c.Quote.FeeCents || fee == 0 || reason != "late_payment" || status != "requested" {
		t.Fatalf("refund row %d %d %s %s", amount, fee, reason, status)
	}
	var payload []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'refund'`).Scan(&payload))
	if string(payload) != `{"refund_id": "`+refund.String()+`", "organizer_id": "`+f.org.String()+`"}` {
		t.Fatalf("job payload %s, want only the two ids", payload)
	}
	m := &mailer.Memory{}
	drain(t, s, m)
	drain(t, s, m) // a second run changes nothing
	if got, gotFee := fake.Refunded(pi.ID); got != c.Quote.TotalCents || gotFee != c.Quote.FeeCents {
		t.Fatalf("Stripe refunded %d and fee %d", got, gotFee)
	}
	in := fake.Refunds()
	if len(in) != 1 || in[0].IdempotencyKey != "refund-"+refund.String()+"-0" || in[0].Metadata["refund_id"] != refund.String() ||
		in[0].Account != "acct_fake1" || in[0].AmountCents != c.Quote.TotalCents {
		t.Fatalf("refunds asked for %+v", in)
	}
	if fr := fake.FeeRefunds(); len(fr) != 1 || fr[0].IdempotencyKey != "appfee-"+refund.String()+"-0" || fr[0].AmountCents != c.Quote.FeeCents {
		t.Fatalf("fee refunds %+v", fr)
	}
	if n := f.count(t, `select count(*) from refunds where status = 'succeeded' and stripe_refund_id like 're_fake%' and fee_refund_id like 'fr_fake%'
		and fee_refunded_at is not null`); n != 1 {
		t.Fatal("the refund row doesn't keep Stripe's ids")
	}
	if _, _, st := f.orderOf(t, c.ID); st != "refunded" {
		t.Fatalf("order %s, want refunded", st)
	}
	// Exactly one buyer email, with P3a's words and no link.
	if len(m.Sent) != 1 || m.Sent[0].To != "late@example.com" || strings.Contains(m.Sent[0].Text, "http") {
		t.Fatalf("emails %+v, want one, with no link", m.Sent)
	}
}

// The buyer's email is queued and sent once, with P3a's words and no link.
func TestLateRefundEmailSaysWhatHappened(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	_, order, _ := latePaid(t, s, f, fake, "late@example.com")
	_, payload := refundOf(t, f, order)
	f.must(t, sales.Refund(s)(ctx, payload))
	var mail []byte
	f.must(t, f.owner.QueryRow(ctx, `select payload from jobs where kind = 'send_email'`).Scan(&mail))
	var msg struct{ To, Subject, Text string }
	f.must(t, json.Unmarshal(mail, &msg))
	if msg.To != "late@example.com" || msg.Subject != "Your refund for Gala" ||
		!strings.Contains(msg.Text, "couldn't keep your tickets for Gala") || !strings.Contains(msg.Text, "so we've refunded you in full: $26.00") ||
		strings.Contains(string(mail), "http") || strings.Contains(string(mail), sales.OrderToken(s.SecretKey, order)) {
		t.Fatalf("email %+v", msg)
	}
}

// Past Stripe's 24-hour idempotency window a retried refund finds the money
// already returned. That completes the refund, and the same for our fee.
func TestRefundFinishesWhenStripeSaysAlreadyRefunded(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	c, order, _ := latePaid(t, s, f, fake, "late@example.com")
	refund, payload := refundOf(t, f, order)
	fake.FailCall("Refund", plugin.ErrAlreadyRefunded)
	f.must(t, sales.Refund(s)(ctx, payload))
	if _, _, st := f.orderOf(t, c.ID); st != "refunded" {
		t.Fatalf("order %s, want refunded", st)
	}
	if n := f.count(t, `select count(*) from refunds where stripe_refund_id is null and status = 'succeeded'`) +
		f.count(t, `select count(*) from jobs where kind = 'send_email'`); n != 2 {
		t.Fatalf("%d of the refund row and the email, want both", n)
	}
	fake.FailCall("RefundApplicationFee", plugin.ErrAlreadyRefunded)
	f.must(t, sales.Refund(s)(ctx, payload))
	if n := f.count(t, `select count(*) from refunds where id = $1 and fee_refunded_at is not null and fee_refund_id is null`, refund); n != 1 {
		t.Fatal("an already refunded fee isn't marked done")
	}
	// Any other error is retried.
	f.exec(t, `update refunds set status = 'requested'`)
	fake.FailNext(errors.New("stripe: refund: boom"))
	if err := sales.Refund(s)(ctx, payload); err == nil {
		t.Fatal("a failed refund reported success")
	}
}

// refundRecorder is the fake provider, keeping every refund request.
type refundRecorder struct {
	*payments.Fake
	mu     sync.Mutex
	inputs []plugin.RefundInput
}

func (r *refundRecorder) Refund(ctx context.Context, in plugin.RefundInput) (plugin.Refund, error) {
	r.mu.Lock()
	r.inputs = append(r.inputs, in)
	r.mu.Unlock()
	return r.Fake.Refund(ctx, in)
}

func (r *refundRecorder) calls() []plugin.RefundInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]plugin.RefundInput(nil), r.inputs...)
}

// Stripe replays a refusal under one idempotency key for 24 hours, longer
// than every retry. A definite refusal moves the refund to its next key;
// anything that might have refunded already (no answer, 5xx, 409, 429) keeps it.
func TestRefundRetriesWithANewKeyOnlyAfterARefusal(t *testing.T) {
	s, _ := newPaidService(t)
	rec := &refundRecorder{Fake: payments.NewFake()}
	s.Payments = rec
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	c, order, _ := latePaid(t, s, f, rec.Fake, "late@example.com")
	refund, payload := refundOf(t, f, order)
	run := sales.Refund(s)

	maybe := []error{
		&plugin.ProviderError{Op: "refund", Cause: "timeout"},
		&plugin.ProviderError{Op: "refund", Status: 500},
		&plugin.ProviderError{Op: "refund", Status: 409},
		&plugin.ProviderError{Op: "refund", Status: 429},
		errors.New("network"),
	}
	for _, e := range maybe {
		rec.FailNext(e)
		if err := run(ctx, payload); err == nil {
			t.Fatalf("%v: reported success", e)
		}
	}
	rec.FailNext(&plugin.ProviderError{Op: "refund", Refused: true, Status: 400, Code: "balance_insufficient"})
	if err := run(ctx, payload); err == nil {
		t.Fatal("a refused refund reported success")
	}
	f.must(t, run(ctx, payload))
	var keys []string
	for _, in := range rec.calls() {
		keys = append(keys, in.IdempotencyKey)
		if in.Metadata["refund_id"] != refund.String() {
			t.Fatalf("refund without its refund_id: %+v", in)
		}
	}
	k0, k1 := "refund-"+refund.String()+"-0", "refund-"+refund.String()+"-1"
	want := []string{k0, k0, k0, k0, k0, k0, k1}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	if _, _, st := f.orderOf(t, c.ID); st != "refunded" {
		t.Fatalf("order %s, want refunded", st)
	}
	// The fee step is queued as a job of its own (same two ids), and keeps
	// to the same rule, from the attempt the row is at.
	if n := f.count(t, `select count(*) from jobs where kind = 'refund' and payload = $1::jsonb`, payload); n != 2 {
		t.Fatalf("%d refund jobs, want the first and the fee step", n)
	}
	fee := payload
	rec.FailCall("RefundApplicationFee", &plugin.ProviderError{Op: "fee refund", Status: 429})
	if err := run(ctx, fee); err == nil {
		t.Fatal("a fee refund told to slow down reported success")
	}
	rec.FailCall("RefundApplicationFee", &plugin.ProviderError{Op: "fee refund", Refused: true, Status: 400})
	if err := run(ctx, fee); err == nil {
		t.Fatal("a refused fee refund reported success")
	}
	f.must(t, run(ctx, fee))
	if fr := rec.FeeRefunds(); len(fr) != 1 || fr[0].IdempotencyKey != "appfee-"+refund.String()+"-2" {
		t.Fatalf("fee refunds %+v, want one under appfee-<refund>-2", fr)
	}
}

func TestNoAccessFailsTheRefundAtOnce(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	_, order, _ := latePaid(t, s, f, fake, "late@example.com")
	fake.FailCall("Refund", &plugin.ProviderError{Op: "refund", Refused: true, NoAccess: true, Status: 403, Code: "account_invalid"})
	drain(t, s, &mailer.Memory{})
	if n := f.count(t, `select count(*) from refunds r join payments p on p.id = r.payment_id
		where p.order_id = $1 and r.status = 'failed' and r.failure = 'no_access'`, order); n != 1 {
		t.Fatal("refund not failed with no_access")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.failed' and actor_system and after->>'failure' = 'no_access'`); n != 1 {
		t.Fatalf("%d refund.failed rows, want 1", n)
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'refund'`); n != 0 {
		t.Fatalf("%d refund jobs left, want none", n)
	}
}

// The account id stays on a disconnected organizer, and on its payments, so
// money still goes back through the account that took it.
func TestRefundStillGoesToADisconnectedAccount(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	_, _, _ = latePaid(t, s, f, fake, "late@example.com")
	f.exec(t, `update organizers set stripe_disconnected_at = now(), stripe_charges_enabled = false where id = $1`, f.org)
	drain(t, s, &mailer.Memory{})
	if in := fake.Refunds(); len(in) != 1 || in[0].Account != "acct_fake1" {
		t.Fatalf("refunds %+v", in)
	}
}

// A pending refund keeps its tickets' seats; succeeded gives them back.
func TestSeatsComeBackOnlyWhenTheRefundSucceeds(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	payment := f.id(t, `select id from payments where order_id = $1`, order)
	// Void one ticket the way Task 4's RefundTickets will: as ts_system here.
	var refund uuid.UUID
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) (err error) {
		refund, err = sales.RequestRefundForTest(ctx, tx, f.org, payment, 2500, 0, false, "organizer", nil)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update tickets set status = 'void', refund_id = $2 where id = $1`, tickets[0], refund)
		return err
	}))
	used := func() (n int) {
		f.must(t, f.owner.QueryRow(ctx, `select quota_used($1, null)`, f.hall).Scan(&n))
		return
	}
	fake.RefundsPending(true)
	drain(t, s, &mailer.Memory{})
	if used() != 2 {
		t.Fatalf("quota used %d while the refund is pending, want 2", used())
	}
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'requested' and stripe_refund_id is not null`, refund); n != 1 {
		t.Fatal("a pending refund isn't waiting with Stripe's id")
	}
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) error { return sales.FinishRefundForTest(s, ctx, tx, f.org, refund, "succeeded") }))
	if used() != 1 {
		t.Fatalf("quota used %d after success, want 1", used())
	}
	if n := f.count(t, `select count(*) from tickets where released_at is not null`); n != 1 {
		t.Fatalf("%d tickets released, want 1", n)
	}
	// Stripe's word again changes nothing: one email, one release.
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) error { return sales.FinishRefundForTest(s, ctx, tx, f.org, refund, "succeeded") }))
	if n := f.count(t, `select count(*) from jobs where kind = 'send_email'`); n != 1 {
		t.Fatalf("%d emails, want 1", n)
	}
	// A failed one keeps its ticket's seat, and says so once.
	var failed uuid.UUID
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) (err error) {
		failed, err = sales.RequestRefundForTest(ctx, tx, f.org, payment, 2500, 0, false, "organizer", nil)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update tickets set status = 'void', refund_id = $2 where id = $1`, tickets[1], failed)
		return err
	}))
	for range 2 {
		f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) error { return sales.FinishRefundForTest(s, ctx, tx, f.org, failed, "failed") }))
	}
	if used() != 1 {
		t.Fatalf("quota used %d after a failed refund, want 1", used())
	}
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'failed' and failure = 'stripe_failed'`, failed) +
		f.count(t, `select count(*) from audit_log where action = 'refund.failed' and entity_id = $1`, failed); n != 2 {
		t.Fatalf("%d of the failed row and its one audit row, want both", n)
	}
}

// A refund that runs out of tries is parked by the queue. It must not stay
// quiet: the refund is marked failed (gave_up), one system audit row says
// so, an ERROR names the order, and nothing in the log names the buyer. A
// crash-parked refund job is reported the same way.
func TestAStuckRefundSurfaces(t *testing.T) {
	for _, crash := range []bool{false, true} {
		name := "ten refusals"
		if crash {
			name = "ten crashes"
		}
		t.Run(name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
			ctx := context.Background()
			_, order, _ := latePaid(t, s, f, fake, "late@example.com")
			refund, _ := refundOf(t, f, order)
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			w := refundWorker(s, &mailer.Memory{})
			failed := func() int {
				return f.count(t, `select count(*) from audit_log where action = 'refund.failed' and actor_system and entity_id = $1
					and after->>'failure' = 'gave_up'`, refund)
			}
			if crash {
				// Its worker died on the tenth run: claimed, never recorded.
				f.exec(t, `update jobs set attempts = 10, locked_until = now() - interval '1 second' where kind = 'refund'`)
				if ran, err := w.RunOnce(ctx); ran || err != nil {
					t.Fatalf("after the crash: %v %v", ran, err)
				}
			} else {
				// Ninth try fails: retried later, nothing raised yet.
				f.exec(t, `update jobs set attempts = 8 where kind = 'refund'`)
				fake.FailNext(&plugin.ProviderError{Op: "refund", Refused: true, Status: 400})
				if ran, err := w.RunOnce(ctx); !ran || err != nil {
					t.Fatalf("ninth try: %v %v", ran, err)
				}
				if failed() != 0 || strings.Contains(logs.String(), "level=ERROR") {
					t.Fatalf("raised before the last try: %d rows, log %s", failed(), logs.String())
				}
				// The tenth, and last, fails too.
				f.exec(t, `update jobs set run_at = now() where kind = 'refund'`)
				fake.FailNext(&plugin.ProviderError{Op: "refund", Refused: true, Status: 400})
				if ran, err := w.RunOnce(ctx); !ran || err != nil {
					t.Fatalf("last try: %v %v", ran, err)
				}
			}
			if n := f.count(t, `select count(*) from jobs where kind = 'refund' and run_at = 'infinity'`); n != 1 {
				t.Fatalf("%d parked refund jobs, want 1", n)
			}
			if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'failed' and failure = 'gave_up'`, refund); n != 1 {
				t.Fatal("the refund isn't failed with gave_up")
			}
			if failed() != 1 {
				t.Fatalf("%d refund.failed rows, want 1", failed())
			}
			got := logs.String()
			if !strings.Contains(got, "level=ERROR") || !strings.Contains(got, order.String()) {
				t.Fatalf("no ERROR naming the order:\n%s", got)
			}
			if strings.Contains(got, "late@example.com") || strings.Contains(got, "secret") {
				t.Fatalf("the log carries the buyer's email or a secret:\n%s", got)
			}
		})
	}
}

// cancelling is the fake provider whose refund is cut short by a shutdown.
type cancelling struct {
	*payments.Fake
	cancel context.CancelFunc
}

func (c *cancelling) Refund(ctx context.Context, _ plugin.RefundInput) (plugin.Refund, error) {
	c.cancel()
	return plugin.Refund{}, ctx.Err()
}

// A shutdown during the last try isn't a stuck refund: the job goes back to
// the queue, with no ERROR and no refund.failed row.
func TestShutdownOnTheLastTryRaisesNothing(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, _ = latePaid(t, s, f, fake, "late@example.com")
	s.Payments = &cancelling{Fake: fake, cancel: cancel}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	f.exec(t, `update jobs set attempts = 9 where kind = 'refund'`)
	if ran, err := refundWorker(s, &mailer.Memory{}).RunOnce(ctx); !ran || err != nil {
		t.Fatalf("last try: %v %v", ran, err)
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.failed'`); n != 0 {
		t.Fatalf("%d refund.failed rows after a shutdown, want 0", n)
	}
	if n := f.count(t, `select count(*) from refunds where status = 'requested'`); n != 1 {
		t.Fatal("a shutdown failed the refund")
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a shutdown logged an ERROR:\n%s", logs.String())
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'refund' and attempts = 9 and run_at <> 'infinity'`); n != 1 {
		t.Fatalf("job not handed back: %d", n)
	}
}

// voidedRefund is a paid two-ticket order with its first ticket voided under
// a requested refund of 2500, written as Task 4's RefundTickets will.
func voidedRefund(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake) (refund uuid.UUID, used func() int) {
	t.Helper()
	ctx := context.Background()
	order, _, tickets := f.paidOrder(t, s, fake, 2, 1)
	payment := f.id(t, `select id from payments where order_id = $1`, order)
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) (err error) {
		refund, err = sales.RequestRefundForTest(ctx, tx, f.org, payment, 2500, 0, false, "organizer", nil)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `update tickets set status = 'void', refund_id = $2 where id = $1`, tickets[0], refund)
		return err
	}))
	return refund, func() (n int) {
		f.must(t, f.owner.QueryRow(ctx, `select quota_used($1, null)`, f.hall).Scan(&n))
		return
	}
}

// crashPark parks every refund job as if its worker died ten times.
func crashPark(t *testing.T, s *sales.Service, f fixture) {
	t.Helper()
	f.exec(t, `update jobs set attempts = 10, locked_until = now() - interval '1 second' where kind = 'refund'`)
	drain(t, s, &mailer.Memory{}) // whatever else is due runs; the refund jobs are parked
	if n := f.count(t, `select count(*) from jobs where kind = 'refund' and run_at <> 'infinity'`); n != 0 {
		t.Fatalf("%d refund jobs not parked", n)
	}
}

// We gave up asking, but the money went out after all: Stripe's word still
// finishes the refund, gives the seat back and tells the buyer, once.
func TestAGaveUpRefundStillFinishesWhenStripeSaysSucceeded(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	ctx := context.Background()
	refund, used := voidedRefund(t, s, f, fake)
	crashPark(t, s, f)
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'failed' and failure = 'gave_up'`, refund); n != 1 {
		t.Fatal("not given up")
	}
	if used() != 2 {
		t.Fatalf("quota used %d after giving up, want 2", used())
	}
	for range 2 {
		f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) error { return sales.FinishRefundForTest(s, ctx, tx, f.org, refund, "succeeded") }))
	}
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'succeeded' and failure is null`, refund); n != 1 {
		t.Fatal("Stripe's succeeded didn't finish a given-up refund")
	}
	if used() != 1 {
		t.Fatalf("quota used %d after success, want 1", used())
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'send_email'`); n != 1 {
		t.Fatalf("%d emails, want 1", n)
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.succeeded_late' and actor_system and entity_id = $1`, refund); n != 1 {
		t.Fatalf("%d refund.succeeded_late rows, want 1", n)
	}
}

// A refund Stripe already has (we hold its id) isn't given up on when its
// job is parked: Stripe's webhook will finish it.
func TestAParkedRefundStripeHasStaysRequested(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 2, price: 2500})
	refund, _ := voidedRefund(t, s, f, fake)
	f.exec(t, `update refunds set stripe_refund_id = 're_waiting' where id = $1`, refund)
	crashPark(t, s, f)
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'requested'`, refund); n != 1 {
		t.Fatal("a refund Stripe has was given up on")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.failed'`); n != 0 {
		t.Fatalf("%d refund.failed rows, want 0", n)
	}
}

// The buyer has their money but our fee step ran out of tries: the refund
// stays succeeded, and one system audit row says the fee is stuck.
func TestAParkedFeeStepIsAudited(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	c, order, _ := latePaid(t, s, f, fake, "late@example.com")
	refund, _ := refundOf(t, f, order)
	fake.FailCall("RefundApplicationFee", &plugin.ProviderError{Op: "fee refund", Refused: true, Status: 400})
	drain(t, s, &mailer.Memory{}) // refund succeeds, the fee step fails once
	f.exec(t, `update jobs set attempts = 9, run_at = now() where kind = 'refund'`)
	fake.FailCall("RefundApplicationFee", &plugin.ProviderError{Op: "fee refund", Refused: true, Status: 400})
	if ran, err := refundWorker(s, &mailer.Memory{}).RunOnce(ctx); !ran || err != nil {
		t.Fatalf("last try: %v %v", ran, err)
	}
	assertFeeStuck(t, f, refund, c.Quote.TotalCents, c.Quote.FeeCents)
}

// A revoked account can't take a fee refund on any try: stuck at once.
func TestNoAccessOnTheFeeStepStopsAtOnce(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	c, order, _ := latePaid(t, s, f, fake, "late@example.com")
	refund, _ := refundOf(t, f, order)
	fake.FailCall("RefundApplicationFee", &plugin.ProviderError{Op: "fee refund", Refused: true, NoAccess: true, Status: 403, Code: "account_invalid"})
	drain(t, s, &mailer.Memory{})
	if n := f.count(t, `select count(*) from jobs where kind = 'refund'`); n != 0 {
		t.Fatalf("%d refund jobs left, want none", n)
	}
	assertFeeStuck(t, f, refund, c.Quote.TotalCents, c.Quote.FeeCents)
}

func assertFeeStuck(t *testing.T, f fixture, refund uuid.UUID, amount, fee int64) {
	t.Helper()
	if n := f.count(t, `select count(*) from refunds where id = $1 and status = 'succeeded' and fee_refunded_at is null`, refund); n != 1 {
		t.Fatal("the refund isn't succeeded with its fee still owed")
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.fee_stuck' and actor_system and entity_id = $1
		and (after->>'amount_cents')::bigint = $2 and (after->>'fee_cents')::bigint = $3`, refund, amount, fee); n != 1 {
		t.Fatalf("%d refund.fee_stuck rows with the amounts, want 1", n)
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.failed'`); n != 0 {
		t.Fatalf("%d refund.failed rows, want 0", n)
	}
}

// Two runs of one job at once (a lock that ran out, say) refund once and
// email once: the same key at Stripe, and the order lock for our side.
func TestTwoRunsOfOneRefundPayOnce(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 1, price: 2500})
	ctx := context.Background()
	_, order, _ := latePaid(t, s, f, fake, "late@example.com")
	_, payload := refundOf(t, f, order)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := sales.Refund(s)(ctx, payload); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := len(fake.Refunds()); n != 1 {
		t.Fatalf("%d Stripe refunds, want 1", n)
	}
	if n := f.count(t, `select count(*) from jobs where kind = 'send_email'`); n != 1 {
		t.Fatalf("%d emails, want 1", n)
	}
}
