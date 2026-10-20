package sales_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// suspend suspends org the way staff_act does, as the owner: a staff
// member, a staff session on org, the suspend action and its row.
func (f fixture) suspend(t *testing.T, org uuid.UUID) {
	t.Helper()
	f.exec(t, `with
		u as (insert into users(email, name, email_verified_at) values (gen_random_uuid() || '@staff.example', 'Staff', now()) returning id),
		st as (insert into staff(user_id, granted_by) select id, 'test' from u returning user_id),
		ss as (insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, expires_at)
			select st.user_id, gen_random_uuid(), o.id, o.slug, o.name, 'Checking a takedown report', now() + interval '1 hour'
			from st, organizers o where o.id = $1 returning id, user_id, organizer_id),
		a as (insert into staff_actions(staff_session_id, user_id, organizer_id, kind, reason)
			select id, user_id, organizer_id, 'suspend', 'Checking a takedown report' from ss returning id, organizer_id)
		insert into organizer_suspensions(organizer_id, staff_action_id) select organizer_id, id from a`, org)
}

func (f fixture) lift(t *testing.T, org uuid.UUID) {
	t.Helper()
	f.exec(t, `delete from organizer_suspensions where organizer_id = $1`, org)
}

// Spec test 24: a suspended organizer's events are gone from the
// storefront, every other organizer's sell on, and a lift brings them back.
// Paid orders keep their links, and a buyer who already paid keeps the
// checkout's road to their order.
func TestSuspendedEventsAreGone(t *testing.T) {
	ctx := context.Background()
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	other := f.organizer(t, "other")
	_, otherGeneral, _ := f.liveEvent(t, other, seedOpts{quota: 10, price: 2500})
	general := sales.Item{TicketTypeID: f.general, Quantity: 1}

	open := f.open(t, s, buy(general))
	done, pi := paying(t, s, f, fake, sales.Details{}, general)
	f.must(t, s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", done.Quote.TotalCents, 1)))
	order, code, _ := f.orderOf(t, done.ID)

	f.suspend(t, f.org)
	_, err := s.StorefrontEvent(ctx, "acme", "gala", "")
	if !errors.Is(err, sales.ErrUnavailable) {
		t.Errorf("StorefrontEvent: %v, want ErrUnavailable", err)
	}
	if _, err := s.CreateCheckout(ctx, buy(general)); !errors.Is(err, sales.ErrUnavailable) {
		t.Errorf("CreateCheckout: %v, want ErrUnavailable", err)
	}
	if _, err := s.GetCheckout(ctx, open.Token); !errors.Is(err, sales.ErrUnavailable) {
		t.Errorf("GetCheckout: %v, want ErrUnavailable", err)
	}
	if _, err := s.Pay(ctx, open.Token, sales.Details{Email: "buyer@example.com"}); !errors.Is(err, sales.ErrUnavailable) {
		t.Errorf("Pay: %v, want ErrUnavailable", err)
	}
	if n := f.count(t, `select count(*) from orders where checkout_id = $1`, open.ID); n != 0 {
		t.Errorf("Pay wrote %d orders", n)
	}
	if got, err := s.GetCheckout(ctx, done.Token); err != nil || got.Status != "completed" || got.OrderCode != code {
		t.Errorf("completed GetCheckout: %q %q %v, want completed %s", got.Status, got.OrderCode, err, code)
	}
	if err := s.RequestOrderLinks(ctx, "acme", "gala", "buyer@example.com"); err != nil {
		t.Errorf("RequestOrderLinks: %v", err)
	}
	if view, err := s.Order(ctx, code, sales.OrderToken(s.SecretKey, order)); err != nil || view.Status != "paid" || len(view.Tickets) != 1 {
		t.Errorf("order link: %q, %d tickets, %v", view.Status, len(view.Tickets), err)
	}

	// Another organizer sells on.
	if _, err := s.StorefrontEvent(ctx, "other", "gala", ""); err != nil {
		t.Errorf("other StorefrontEvent: %v", err)
	}
	in := buy(sales.Item{TicketTypeID: otherGeneral, Quantity: 1})
	in.Organizer = "other"
	if _, err := s.CreateCheckout(ctx, in); err != nil {
		t.Errorf("other CreateCheckout: %v", err)
	}

	f.lift(t, f.org)
	if _, err := s.StorefrontEvent(ctx, "acme", "gala", ""); err != nil {
		t.Errorf("StorefrontEvent after lift: %v", err)
	}
	if _, err := s.GetCheckout(ctx, open.Token); err != nil {
		t.Errorf("GetCheckout after lift: %v", err)
	}
	if _, err := s.Pay(ctx, open.Token, sales.Details{Email: "buyer@example.com"}); err != nil {
		t.Errorf("Pay after lift: %v", err)
	}
	f.open(t, s, buy(general))
}

// A free order is paid inside Pay, so it never gets past the check.
func TestSuspendedFreeOrderIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _ := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10})
	c := f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 1}))
	f.suspend(t, f.org)
	if _, err := s.Pay(ctx, c.Token, sales.Details{Email: "buyer@example.com"}); !errors.Is(err, sales.ErrUnavailable) {
		t.Fatalf("Pay: %v, want ErrUnavailable", err)
	}
	if n := f.count(t, `select count(*) from orders`) + f.count(t, `select count(*) from tickets`); n != 0 {
		t.Fatalf("%d orders or tickets written", n)
	}
}

// Spec test 25: a payment that lands after the suspension, by the webhook
// or the settle job, is cancelled and refunded in full, our fee included.
func TestPaymentAfterSuspendIsRefunded(t *testing.T) {
	ctx := context.Background()
	for name, confirm := range map[string]func(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake, c sales.Checkout, pi plugin.PaymentIntent){
		"webhook": func(t *testing.T, s *sales.Service, _ fixture, _ *payments.Fake, c sales.Checkout, pi plugin.PaymentIntent) {
			if err := s.HandleStripeEvent(ctx, succeeded(pi.ID, "acct_fake1", c.Quote.TotalCents, 1)); err != nil {
				t.Fatal(err)
			}
		},
		"settle job": func(t *testing.T, s *sales.Service, f fixture, fake *payments.Fake, c sales.Checkout, pi plugin.PaymentIntent) {
			f.exec(t, `update checkouts set expires_at = now() - interval '2 hours' where id = $1`, c.ID)
			f.must(t, jobs.Sweep(ctx, s.Pool))
			fake.SetIntentStatus(pi.ID, "succeeded")
			drain(t, s, &mailer.Memory{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
			c, pi := paying(t, s, f, fake, sales.Details{}, sales.Item{TicketTypeID: f.general, Quantity: 1})
			f.suspend(t, f.org)
			confirm(t, s, f, fake, c, pi)

			order, _, status := f.orderOf(t, c.ID)
			want := "cancelled"
			if name == "settle job" {
				want = "refunded" // its drain runs the refund too
			}
			if status != want {
				t.Fatalf("order %s, want %s", status, want)
			}
			if n := f.count(t, `select count(*) from tickets where order_id = $1`, order); n != 0 {
				t.Errorf("%d tickets cut", n)
			}
			if n := f.count(t, `select count(*) from refunds r join payments p on p.id = r.payment_id
				where p.order_id = $1 and r.reason = 'late_payment' and r.amount_cents = $2 and r.fee_refunded and r.fee_cents = $3`,
				order, c.Quote.TotalCents, c.Quote.FeeCents); n != 1 || c.Quote.FeeCents == 0 {
				t.Errorf("%d full late_payment refunds with the fee of %d, want 1", n, c.Quote.FeeCents)
			}
			if n := f.count(t, `select count(*) from refunds`); n != 1 {
				t.Errorf("%d refunds, want 1", n)
			}
			if n := f.count(t, `select count(*) from audit_log where action = 'order.cancelled_late' and entity_id = $1
				and actor_system and after->>'reason' = 'organizer_suspended'`, order); n != 1 {
				t.Errorf("%d order.cancelled_late audits with reason organizer_suspended, want 1", n)
			}
		})
	}
}
