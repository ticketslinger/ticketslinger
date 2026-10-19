package sales_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/pricing"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
)

// ownAccount is the fake as the direct Stripe provider: every call it gets
// is recorded with the account it named, and there's no fee to refund.
type ownAccount struct {
	*payments.Fake
	mu    sync.Mutex
	calls []string // "Method account"
}

func (o *ownAccount) saw(method, account string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, method+" "+account)
}

func (o *ownAccount) CreatePaymentIntent(ctx context.Context, in plugin.PaymentIntentInput) (plugin.PaymentIntent, error) {
	o.saw("CreatePaymentIntent", in.Account)
	return o.Fake.CreatePaymentIntent(ctx, in)
}

func (o *ownAccount) Refund(ctx context.Context, in plugin.RefundInput) (plugin.Refund, error) {
	o.saw("Refund", in.Account)
	return o.Fake.Refund(ctx, in)
}

func (o *ownAccount) ListRefunds(ctx context.Context, account, intent string) ([]plugin.Refund, error) {
	o.saw("ListRefunds", account)
	return o.Fake.ListRefunds(ctx, account, intent)
}

func (o *ownAccount) PaymentIntentStatus(ctx context.Context, account, intent string) (string, error) {
	o.saw("PaymentIntentStatus", account)
	return o.Fake.PaymentIntentStatus(ctx, account, intent)
}

func (o *ownAccount) CancelPaymentIntent(ctx context.Context, account, intent, key string) error {
	o.saw("CancelPaymentIntent", account)
	return o.Fake.CancelPaymentIntent(ctx, account, intent, key)
}

func (o *ownAccount) RefundApplicationFee(_ context.Context, in plugin.FeeRefundInput) (string, error) {
	o.saw("RefundApplicationFee", in.Account)
	return "", plugin.ErrDirectPayments
}

func (o *ownAccount) Account(_ context.Context, account string) (plugin.AccountStatus, error) {
	o.saw("Account", account)
	return plugin.AccountStatus{}, plugin.ErrDirectPayments
}

func (o *ownAccount) seen() map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	m := map[string]int{}
	for _, c := range o.calls {
		m[c]++
	}
	return m
}

// A self-hosted server sells through its own Stripe account: no organizer
// connects anything, no fee is taken, and every payment, webhook, refund and
// cancellation finds its order with the account left empty.
func TestDirectModeSellsWithoutConnect(t *testing.T) {
	s := newService(t)
	s.Direct, s.Fee = true, pricing.Fee{}
	own := &ownAccount{Fake: payments.NewFake()}
	s.Payments = own
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	f.exec(t, `update organizers set stripe_account_id = null, stripe_link_method = null, stripe_charges_enabled = false where id = $1`, f.org)
	ctx := context.Background()

	// Paid, through the server's own account.
	c, pi := paying(t, s, f, own.Fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 2})
	var account string
	var fee int64
	f.must(t, f.owner.QueryRow(ctx, `select stripe_account_id, application_fee_cents from payments where payment_intent_id = $1`, pi.ID).Scan(&account, &fee))
	if account != "" || fee != 0 || c.Quote.TotalCents != 5000 {
		t.Fatalf("payment on account %q with fee %d, total %d; want '', 0, 5000", account, fee, c.Quote.TotalCents)
	}
	var routed *uuid.UUID
	f.must(t, s.Pool.QueryRow(ctx, `select organizer_for_payment($1, '')`, pi.ID).Scan(&routed))
	if routed == nil || *routed != f.org {
		t.Fatalf("organizer_for_payment(pi, '') = %v, want %s", routed, f.org)
	}
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "", c.Quote.TotalCents, 1)))
	order, _, status := f.orderOf(t, c.ID)
	if status != "paid" {
		t.Fatalf("order %s after the webhook, want paid", status)
	}
	// Another buyer still paying when the event is cancelled.
	pending, pendingPI := paying(t, s, f, own.Fake, sales.Details{Email: "late@example.com"}, sales.Item{TicketTypeID: f.general, Quantity: 1})

	// The organizer refunds one ticket, asking for a fee back there isn't.
	var tickets []uuid.UUID
	rows, err := f.owner.Query(ctx, `select id from tickets where order_id = $1 order by id`, order)
	f.must(t, err)
	tickets, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	f.must(t, err)
	res := f.refund(t, s, order, sales.RefundRequest{TicketIDs: tickets[:1], FeeBack: true})
	if res.AmountCents != 2500 || res.FeeCents != 0 || res.RefundID == nil {
		t.Fatalf("refund %+v, want 2500 and no fee", res)
	}
	own.RefundsPending(true)
	drain(t, s, &mailer.Memory{})
	own.RefundsPending(false)
	var stripeID string
	f.must(t, f.owner.QueryRow(ctx, `select stripe_refund_id from refunds where id = $1`, *res.RefundID).Scan(&stripeID))
	f.must(t, s.HandleStripeEvent(ctx, refundEvent(stripeID, pi.ID, "", 2500, "succeeded", ours(*res.RefundID, 0), 2)))
	if st := f.str(t, `select status from refunds where id = $1`, *res.RefundID); st != "succeeded" {
		t.Fatalf("refund %s after its webhook, want succeeded", st)
	}
	if n := f.count(t, `select count(*) from jobs where kind = $1`, sales.RefundJob); n != 0 {
		t.Fatalf("%d refund jobs queued after the refund succeeded, want no fee step", n)
	}

	// The event is cancelled: the rest of the paid order goes back, and the
	// payment still in flight is stopped.
	_, err = f.cancel(s, "Gala")
	f.must(t, err)
	drain(t, s, &mailer.Memory{})
	if amount, _ := own.Refunded(pi.ID); amount != 5000 {
		t.Fatalf("refunded %d of the paid order, want 5000", amount)
	}
	if !own.Cancelled(pendingPI.ID) {
		t.Fatal("the payment still in flight wasn't cancelled")
	}
	if _, _, st := f.orderOf(t, pending.ID); st != "cancelled" {
		t.Fatalf("pending order %s, want cancelled", st)
	}

	seen := own.seen()
	for _, want := range []string{"CreatePaymentIntent ", "Refund ", "PaymentIntentStatus ", "CancelPaymentIntent "} {
		if seen[want] == 0 {
			t.Errorf("no %q call", want)
		}
	}
	for call := range seen {
		if call[len(call)-1] != ' ' {
			t.Errorf("Stripe call %q named an account", call)
		}
		if call == "RefundApplicationFee " || call == "Account " {
			t.Errorf("direct mode called %s", call)
		}
	}
}

// A refund row owing a fee (one made before the server switched to direct
// mode, say) finishes without the fee step: there's no fee to give back, so
// the step is skipped, never retried into a parked job.
func TestDirectModeSkipsTheFeeStep(t *testing.T) {
	s := newService(t)
	s.Direct, s.Fee = true, pricing.Fee{}
	own := &ownAccount{Fake: payments.NewFake()}
	s.Payments = own
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	c, pi := paying(t, s, f, own.Fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "", c.Quote.TotalCents, 1)))
	payment := f.id(t, `select id from payments where payment_intent_id = $1`, pi.ID)
	var refund uuid.UUID
	f.must(t, system.Run(ctx, s.Pool, f.org, func(tx pgx.Tx) (err error) {
		refund, err = sales.RequestRefundForTest(ctx, tx, f.org, payment, 2500, 100, true, "organizer", nil)
		return err
	}))
	drain(t, s, &mailer.Memory{})
	if st := f.str(t, `select status from refunds where id = $1`, refund); st != "succeeded" {
		t.Fatalf("refund %s, want succeeded", st)
	}
	if n := own.seen()["RefundApplicationFee "]; n != 0 {
		t.Fatalf("%d fee refunds asked for in direct mode", n)
	}
	if n := f.count(t, `select count(*) from jobs`); n != 0 {
		t.Fatalf("%d jobs left waiting to retry", n)
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'refund.fee_stuck'`); n != 0 {
		t.Fatalf("%d fee_stuck rows", n)
	}
}
