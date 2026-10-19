package sales_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

// failing answers each Send with the next error in errs, then delivers.
type failing struct {
	mailer.Memory
	errs []error
}

func (m *failing) Send(ctx context.Context, msg mailer.Message) error {
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		return err
	}
	return m.Memory.Send(ctx, msg)
}

type mailRow struct {
	status, problem *string
	attempts        int
}

func (r mailRow) String() string {
	s := func(p *string) string {
		if p == nil {
			return "null"
		}
		return *p
	}
	return fmt.Sprintf("%s/%s/%d", s(r.status), s(r.problem), r.attempts)
}

func (f fixture) mail(t *testing.T, order uuid.UUID) (r mailRow) {
	t.Helper()
	f.must(t, f.owner.QueryRow(context.Background(), `select mail_status, mail_problem, mail_attempts from orders where id = $1`, order).
		Scan(&r.status, &r.problem, &r.attempts))
	return r
}

func (f fixture) wantMail(t *testing.T, order uuid.UUID, status, problem string, attempts int) {
	t.Helper()
	r := f.mail(t, order)
	if r.status == nil && status != "" || r.status != nil && *r.status != status ||
		r.problem == nil && problem != "" || r.problem != nil && *r.problem != problem || r.attempts != attempts {
		t.Fatalf("mail %v, want %s/%s/%d", r, status, problem, attempts)
	}
}

// emailWorker runs only ticket emails, with the parked hook main registers.
func emailWorker(s *sales.Service, m mailer.Mailer) *jobs.Worker {
	return &jobs.Worker{Pool: s.Pool, Handlers: map[string]jobs.Handler{sales.OrderConfirmationJob: s.OrderConfirmation(m)},
		Parked: map[string]func(context.Context, []byte, string){sales.OrderConfirmationJob: sales.TicketEmailParked(s)}}
}

func runOne(t *testing.T, w *jobs.Worker) {
	t.Helper()
	ran, err := w.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("ran %v, %v", ran, err)
	}
}

func provider(kind mailer.Kind, status int, problem string) *mailer.Error {
	return &mailer.Error{Provider: "resend", Kind: kind, Status: status, Code: "test", Problem: problem}
}

func TestTicketEmailStatusFollowsTheSend(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	f.wantMail(t, order, "queued", "", 0)

	down := provider(mailer.Retry, 503, "unreachable")
	m := &failing{errs: []error{down, down}}
	w := emailWorker(s, m)
	runOne(t, w)
	f.wantMail(t, order, "failing", "unreachable", 1)
	for range 2 {
		f.exec(t, `update jobs set run_at = now() where kind = 'order_confirmation'`)
		runOne(t, w)
	}
	f.wantMail(t, order, "sent", "", 3)
	if len(m.Sent) != 1 {
		t.Fatalf("%d emails, want 1", len(m.Sent))
	}

	// A daily quota on the last attempt waits for tomorrow: it isn't a give-up.
	order2, _, _ := f.paidOrder(t, s, fake, 1, 2)
	f.exec(t, `update jobs set attempts = 9 where kind = 'order_confirmation'`)
	runOne(t, emailWorker(s, &failing{errs: []error{provider(mailer.DailyQuota, 429, "quota")}}))
	f.wantMail(t, order2, "failing", "quota", 10)
	if n := f.count(t, `select count(*) from jobs where kind = 'order_confirmation' and run_at <> 'infinity'`); n != 1 {
		t.Fatalf("%d waiting jobs after a quota answer, want 1 (not parked)", n)
	}
}

func TestPermanentFailureGivesUpAtOnce(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	runOne(t, emailWorker(s, &failing{errs: []error{provider(mailer.Permanent, 422, "bad_address")}}))
	f.wantMail(t, order, "gave_up", "bad_address", 1)
	if n := f.count(t, `select count(*) from jobs where kind = 'order_confirmation' and run_at = 'infinity'`); n != 1 {
		t.Fatalf("%d parked jobs, want 1", n)
	}
}

func TestCrashParkedEmailGivesUp(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	// Its tenth run took the worker down: claimed, never finished.
	f.exec(t, `update jobs set attempts = 10, locked_until = now() - interval '1 second' where kind = 'order_confirmation'`)
	if ran, err := emailWorker(s, &mailer.Memory{}).RunOnce(context.Background()); ran || err != nil {
		t.Fatalf("ran %v, %v", ran, err)
	}
	f.wantMail(t, order, "gave_up", "", 10)
}

// The parked hook counts the tries actually made: a job parked on its first
// run (its own status write lost) gave up after one try, not ten.
func TestParkedEmailCountsItsTries(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	w := emailWorker(s, nil)
	w.Handlers[sales.OrderConfirmationJob] = func(context.Context, []byte) error {
		return jobs.Permanent(errors.New("status write lost"))
	}
	runOne(t, w)
	f.wantMail(t, order, "gave_up", "", 1)
}

func TestQueueAnswerUnknownWordIsAnError(t *testing.T) {
	for word, want := range map[string]string{"on_its_way": "email_on_its_way", "too_soon": "too_soon", "no_tickets": "no_tickets"} {
		if err := sales.QueueAnswerForTest(&word); conflictCode(err) != want {
			t.Errorf("%s: %v, want %s", word, err, want)
		}
	}
	queued, odd := "queued", "sent_by_pigeon"
	if err := sales.QueueAnswerForTest(&queued); err != nil {
		t.Errorf("queued: %v", err)
	}
	if err := sales.QueueAnswerForTest(&odd); err == nil || conflictCode(err) != "" {
		t.Errorf("an unknown answer: %v, want a plain error", err)
	}
	if err := sales.QueueAnswerForTest(nil); !errors.Is(err, errNotFound) {
		t.Errorf("null: %v, want ErrNotFound", err)
	}
}

// An order the job finds with nothing to send must not stay "on its way",
// or nobody could ever send it again.
func TestNothingToSendClearsTheStatus(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	voided, _, _ := f.paidOrder(t, s, fake, 1, 1)
	f.exec(t, `update tickets set status = 'void' where order_id = $1`, voided)
	refunded, _, _ := f.paidOrder(t, s, fake, 1, 2)
	f.exec(t, `update orders set status = 'refunded' where id = $1`, refunded)
	f.exec(t, `update orders set mail_status = 'failing' where id = $1`, refunded)
	m := &mailer.Memory{}
	runKind(t, s, sales.OrderConfirmationJob, s.OrderConfirmation(m))
	if len(m.Sent) != 0 {
		t.Fatalf("sent %d emails", len(m.Sent))
	}
	f.wantMail(t, voided, "", "", 0)
	f.wantMail(t, refunded, "", "", 0)
}

func TestResendTicketEmail(t *testing.T) {
	for name, resend := range map[string]func(s *sales.Service, f fixture, order uuid.UUID) error{
		"ResendTicketEmail": func(s *sales.Service, f fixture, order uuid.UUID) error {
			return db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
				st, err := sales.ResendTicketEmail(context.Background(), tx, f.org, f.event, order, audit.Actor{User: f.user})
				if err == nil && (st.Status == nil || *st.Status != "queued" || st.UpdatedAt == nil) {
					return errors.New("resent, but the state isn't queued")
				}
				return err
			})
		},
		"queueTicketEmail": func(s *sales.Service, f fixture, order uuid.UUID) error {
			return db.WithOrganizer(context.Background(), s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
				return sales.QueueTicketEmailForTest(context.Background(), tx, order)
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, fake := newPaidService(t)
			f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
			order, _, _ := f.paidOrder(t, s, fake, 1, 1)
			drain(t, s, &mailer.Memory{})
			f.wantMail(t, order, "sent", "", 1)

			f.exec(t, `update orders set mail_requested_at = now() - interval '11 minutes' where id = $1`, order)
			f.must(t, resend(s, f, order))
			f.wantMail(t, order, "queued", "", 0)
			if n := f.count(t, `select count(*) from jobs where kind = 'order_confirmation' and payload->>'order_id' = $1`, order.String()); n != 1 {
				t.Fatalf("%d jobs, want 1", n)
			}
			audits := f.count(t, `select count(*) from audit_log where action = 'order.email_resent' and entity_id = $1`, order)
			if want := map[bool]int{true: 1, false: 0}[name == "ResendTicketEmail"]; audits != want {
				t.Fatalf("%d audit rows, want %d", audits, want)
			}

			var w catalogue.ErrConflict
			if err := resend(s, f, order); !errors.As(err, &w) || w.Code != "email_on_its_way" ||
				w.Message != "We're still trying to send the latest one. Give it a little while." {
				t.Fatalf("again at once: %v, want email_on_its_way", err)
			}
			f.exec(t, `update orders set mail_status = 'sent', mail_requested_at = now() - interval '5 minutes' where id = $1`, order)
			var c catalogue.ErrConflict
			if err := resend(s, f, order); !errors.As(err, &c) || c.Code != "too_soon" ||
				c.Message != "These were sent less than 10 minutes ago. Wait until it's been 10 minutes, then try again." {
				t.Fatalf("5 minutes on: %v, want too_soon", err)
			}
			f.exec(t, `update orders set status = 'refunded', mail_requested_at = now() - interval '11 minutes' where id = $1`, order)
			if err := resend(s, f, order); conflictCode(err) != "no_tickets" {
				t.Fatalf("refunded: %v, want no_tickets", err)
			}

			other := f.organizer(t, "other")
			event, _, _ := f.liveEvent(t, other, seedOpts{quota: 5, price: 2500})
			theirs := f.id(t, `insert into orders (organizer_id, event_id, code, email, status, subtotal_cents, discount_cents, tax_cents, fee_cents,
					total_cents, fee_mode, quote, details, paid_at)
				values ($1, $2, 'TS-ZZZZZ', 'x@example.com', 'paid', 2500, 0, 0, 100, 2600, 'pass', '{}', '{}', now()) returning id`, other, event)
			if err := resend(s, f, theirs); !errors.Is(err, errNotFound) {
				t.Fatalf("another organizer's order: %v, want ErrNotFound", err)
			}
		})
	}
}

// Resend's word about an email from before the latest send request is about
// an older send: it leaves the order alone. One about the latest moves it.
func TestOldMailEventsAreIgnored(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	order, _, _ := f.paidOrder(t, s, fake, 1, 1)
	f.wantMail(t, order, "queued", "", 0)
	var requested time.Time
	f.must(t, f.owner.QueryRow(context.Background(), `select mail_requested_at from orders where id = $1`, order).Scan(&requested))

	f.must(t, s.MailEvent(context.Background(), f.org, order, "delivered", requested.Add(-15*time.Minute)))
	f.wantMail(t, order, "queued", "", 0)
	f.must(t, s.MailEvent(context.Background(), f.org, order, "bounced", requested.Add(-15*time.Minute)))
	f.wantMail(t, order, "queued", "", 0)

	// Resend's clock may run a little behind ours.
	f.must(t, s.MailEvent(context.Background(), f.org, order, "delivered", requested.Add(-30*time.Second)))
	f.wantMail(t, order, "delivered", "", 0)

	// Complained is the last word: a late bounced doesn't take it back.
	f.must(t, s.MailEvent(context.Background(), f.org, order, "complained", requested.Add(time.Second)))
	f.must(t, s.MailEvent(context.Background(), f.org, order, "bounced", requested.Add(time.Second)))
	f.wantMail(t, order, "complained", "", 0)
	if err := s.MailEvent(context.Background(), f.org, order, "sent", requested.Add(time.Second)); err == nil {
		t.Fatal("a kind Resend doesn't send was accepted")
	}
}

// A job the queue gave up on whose status write was lost (the handler's and
// the parked hook's both) leaves the order queued with nothing on its way:
// sending again must still work, for the organizer and for the buyer.
func TestParkedEmailWithAStuckStatusCanBeSentAgain(t *testing.T) {
	s, fake := newPaidService(t)
	f := seed(t, s.Pool, seedOpts{quota: 5, price: 2500})
	ctx := context.Background()
	stuck := func(n int) uuid.UUID { // the only job left: this order's
		order, _, _ := f.paidOrder(t, s, fake, 1, n)
		w := emailWorker(s, nil)
		w.Handlers[sales.OrderConfirmationJob] = func(context.Context, []byte) error { return errors.New("status write lost") }
		parked := w.Parked[sales.OrderConfirmationJob]
		w.Parked[sales.OrderConfirmationJob] = func(ctx context.Context, payload []byte, cause string) {
			gone, cancel := context.WithCancel(ctx)
			cancel() // its update fails
			parked(gone, payload, cause)
		}
		f.exec(t, `delete from jobs where not (kind = 'order_confirmation' and payload->>'order_id' = $1)`, order.String())
		for f.count(t, `select count(*) from jobs where run_at = 'infinity'`) == 0 {
			f.exec(t, `update jobs set run_at = now()`)
			runOne(t, w)
		}
		f.wantMail(t, order, "queued", "", 0)
		f.exec(t, `update orders set mail_requested_at = now() - interval '11 minutes' where id = $1`, order)
		return order
	}

	order := stuck(1)
	f.must(t, db.WithOrganizer(ctx, s.Pool, f.user, f.org, func(tx pgx.Tx, _ string) error {
		_, err := sales.ResendTicketEmail(ctx, tx, f.org, f.event, order, audit.Actor{User: f.user})
		return err
	}))
	f.wantMail(t, order, "queued", "", 0)

	order = stuck(2)
	f.must(t, s.RequestTicketEmail(ctx, f.str(t, `select code from orders where id = $1`, order), sales.OrderToken(s.SecretKey, order)))
	if n := f.count(t, `select count(*) from jobs where kind = 'order_confirmation' and payload->>'order_id' = $1 and run_at <> 'infinity'`, order.String()); n != 1 {
		t.Fatalf("%d live jobs after the buyer's request, want 1", n)
	}
}
