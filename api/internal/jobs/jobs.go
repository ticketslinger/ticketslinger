// Package jobs is a small PostgreSQL job queue and the housekeeping sweep.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

// Handler runs one job. Delivery is at least once: a crash, a timeout or a
// failed outcome write runs the job again, so handlers must be idempotent.
// The worker uses the app pool: work on tenant data must go through
// system.Run for the organizer it acts for, never around it.
type Handler func(ctx context.Context, payload []byte) error

// Enqueue adds a job inside tx, so it exists only if tx commits.
func Enqueue(ctx context.Context, tx pgx.Tx, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `insert into jobs(kind, payload) values ($1, $2)`, kind, b)
	return err
}

// MaxAttempts is how often a job runs before it's parked (claim and fail
// say the same in SQL).
const MaxAttempts = 10

type attemptKey struct{}

// Attempt is which run of its job this is, from 1, inside a Handler; 0
// outside one. At MaxAttempts a failure parks the job.
func Attempt(ctx context.Context) int {
	n, _ := ctx.Value(attemptKey{}).(int)
	return n
}

type Worker struct {
	Pool     *pgxpool.Pool
	Handlers map[string]Handler
	Poll     time.Duration // default 1s
}

const claim = `update jobs set locked_until = now() + interval '5 minutes', attempts = attempts + 1
	where id = (select id from jobs where run_at <= now() and attempts < 10 and (locked_until is null or locked_until < now())
	            order by run_at, id limit 1 for update skip locked)
	returning id, kind, payload, attempts`

const fail = `update jobs set run_at = case when attempts >= 10 then 'infinity'::timestamptz
	else now() + least(interval '1 minute' * power(2, attempts), interval '6 hours') end,
	last_error = left($2, 2000), locked_until = null where id = $1`

// Run claims and runs due jobs until ctx ends. A failed job is retried with
// backoff min(2^attempts minutes, 6h); after 10 attempts it stays with
// run_at = 'infinity' and last_error set, for the organizer to see later.
// A separate goroutine calls Sweep at start and every minute, so a busy
// queue can't starve it.
func (w *Worker) Run(ctx context.Context) {
	poll := w.Poll
	if poll <= 0 {
		poll = time.Second
	}
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			if err := Sweep(ctx, w.Pool); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "jobs: sweep", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	defer func() { <-swept }()
	for ctx.Err() == nil {
		ran, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "jobs: run", "err", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}

// RunOnce claims and runs at most one due job; reports whether it ran one.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	var (
		id       string
		kind     string
		payload  []byte
		attempts int
	)
	err := w.Pool.QueryRow(ctx, claim).Scan(&id, &kind, &payload, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		// A job that crashed its worker ten times is poison: park it.
		_, err = w.Pool.Exec(ctx, `update jobs set run_at = 'infinity', locked_until = null, last_error = 'gave up after a crash'
			where attempts >= 10 and run_at <> 'infinity' and locked_until < now()`)
		return false, err
	}
	if err != nil {
		return false, err
	}
	herr := w.handle(context.WithValue(ctx, attemptKey{}, attempts), kind, payload)
	// A shutdown mid-job must still record the outcome.
	rec := context.WithoutCancel(ctx)
	if herr != nil && ctx.Err() != nil {
		// Cancelled, not broken: hand the job back as if never claimed.
		_, err = w.Pool.Exec(rec, `update jobs set attempts = attempts - 1, run_at = now(), locked_until = null where id = $1`, id)
	} else if herr != nil {
		log := slog.WarnContext
		if attempts >= MaxAttempts {
			log = slog.ErrorContext // parked: nothing runs it again
		}
		log(ctx, "jobs: failed", "kind", kind, "id", id, "attempt", attempts, "parked", attempts >= MaxAttempts, "err", herr)
		_, err = w.Pool.Exec(rec, fail, id, herr.Error())
	} else {
		_, err = w.Pool.Exec(rec, `delete from jobs where id = $1`, id)
	}
	if err != nil {
		err = fmt.Errorf("jobs: record outcome: %w", err)
	}
	return true, err
}

func (w *Worker) handle(ctx context.Context, kind string, payload []byte) (err error) {
	h, ok := w.Handlers[kind]
	if !ok {
		return fmt.Errorf("no handler for kind %s", kind)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked (%T)", r)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return h(ctx, payload)
}

const sweepBatch = 1000

// Sweep deletes expired sessions, webauthn challenges, email tokens,
// idempotency keys and Stripe link states, failed sign-in counts untouched
// for a day, and old abandoned checkouts, a batch at a time. Every step runs; the errors
// come back joined.
func Sweep(ctx context.Context, pool *pgxpool.Pool) error {
	var errs []error
	for _, q := range []string{
		`delete from sessions where id in (select id from sessions where expires_at < now() or created_at < now() - interval '90 days' limit $1)`,
		`delete from webauthn_challenges where id in (select id from webauthn_challenges where expires_at < now() limit $1)`,
		`delete from email_tokens where token_hash in (select token_hash from email_tokens where expires_at < now() limit $1)`,
		`delete from idempotency_keys where (scope, key) in (select scope, key from idempotency_keys where expires_at < now() limit $1)`,
		`delete from login_failures where (account, ip) in (select account, ip from login_failures where updated_at < now() - interval '1 day' limit $1)`,
	} {
		for {
			tag, err := pool.Exec(ctx, q, sweepBatch)
			if err != nil {
				errs = append(errs, err)
				break
			}
			if tag.RowsAffected() < sweepBatch {
				break
			}
		}
	}
	// Tenant tables the sweep can't see through RLS purge through a function.
	for _, fn := range []string{`select purge_expired_checkouts($1)`, `select purge_expired_link_states($1)`} {
		for {
			var n int
			if err := pool.QueryRow(ctx, fn, sweepBatch).Scan(&n); err != nil {
				errs = append(errs, err)
				break
			}
			if n < sweepBatch {
				break
			}
		}
	}
	return errors.Join(errs...)
}

// SendEmail handles the send_email job: payload {"to","subject","text"}.
func SendEmail(m mailer.Mailer) Handler {
	return func(ctx context.Context, payload []byte) error {
		var p struct{ To, Subject, Text string }
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		return m.Send(ctx, p.To, p.Subject, p.Text)
	}
}
