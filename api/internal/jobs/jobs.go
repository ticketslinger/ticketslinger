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

type idKey struct{}

// ID is the running job's own id (a uuid as text) inside a Handler; "" outside one.
func ID(ctx context.Context) string {
	s, _ := ctx.Value(idKey{}).(string)
	return s
}

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// Permanent marks err as one no retry can fix: the job is parked at once,
// with err as its last_error and the Parked hook's cause.
// Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

type delayError struct {
	error
	until time.Time
}

func (e delayError) Unwrap() error { return e.error }

// Delay asks for the job to run again at until (never more than 25 hours
// out) without spending an attempt, for a provider that said "not now". It
// holds only while the job is under 72 hours old; after that it is an
// ordinary failure.
// Delay(until, nil) is nil.
func Delay(until time.Time, err error) error {
	if err == nil {
		return nil
	}
	return delayError{err, until}
}

type Worker struct {
	Pool     *pgxpool.Pool
	Handlers map[string]Handler
	// Parked is told about each job the queue gives up on, by kind: after
	// MaxAttempts failures, or after its worker crashed MaxAttempts times.
	// The cause is the last error. Runs outside any transaction; it must not
	// fail the queue.
	Parked map[string]func(ctx context.Context, payload []byte, cause string)
	Poll   time.Duration // default 1s
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
		const crashed = "gave up after a crash"
		rows, _ := w.Pool.Query(ctx, `update jobs set run_at = 'infinity', locked_until = null, last_error = $1
			where attempts >= 10 and run_at <> 'infinity' and locked_until < now() returning kind, payload`, crashed)
		// Collected first, so the update has committed before any hook runs.
		parked, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
			Kind    string
			Payload []byte
		}])
		for _, p := range parked {
			w.park(ctx, p.Kind, p.Payload, crashed)
		}
		return false, err
	}
	if err != nil {
		return false, err
	}
	herr := w.handle(context.WithValue(context.WithValue(ctx, attemptKey{}, attempts), idKey{}, id), kind, payload)
	// A shutdown mid-job must still record the outcome.
	rec := context.WithoutCancel(ctx)
	if herr != nil && ctx.Err() != nil {
		// Cancelled, not broken: hand the job back as if never claimed.
		_, err = w.Pool.Exec(rec, `update jobs set attempts = attempts - 1, run_at = now(), locked_until = null where id = $1`, id)
	} else if perm := (permanentError{}); errors.As(herr, &perm) {
		slog.ErrorContext(ctx, "jobs: failed for good", "kind", kind, "id", id, "attempt", attempts, "parked", true, "err", herr)
		_, err = w.Pool.Exec(rec, `update jobs set attempts = $3, run_at = 'infinity', last_error = left($2, 2000), locked_until = null where id = $1`, id, herr.Error(), MaxAttempts)
		if err == nil {
			w.park(rec, kind, payload, herr.Error())
		}
	} else if del := (delayError{}); errors.As(herr, &del) && w.delay(rec, id, del.until, herr.Error(), &err) {
		slog.WarnContext(ctx, "jobs: delayed", "kind", kind, "id", id, "attempt", attempts, "err", herr)
	} else if herr != nil {
		log := slog.WarnContext
		if attempts >= MaxAttempts {
			log = slog.ErrorContext // parked: nothing runs it again
		}
		log(ctx, "jobs: failed", "kind", kind, "id", id, "attempt", attempts, "parked", attempts >= MaxAttempts, "err", herr)
		_, err = w.Pool.Exec(rec, fail, id, herr.Error())
		if err == nil && attempts >= MaxAttempts {
			w.park(rec, kind, payload, herr.Error())
		}
	} else {
		_, err = w.Pool.Exec(rec, `delete from jobs where id = $1`, id)
	}
	if err != nil {
		err = fmt.Errorf("jobs: record outcome: %w", err)
	}
	return true, err
}

// delay reschedules a young job without counting the try; false if the job
// is too old, which leaves it to the ordinary failure path.
func (w *Worker) delay(ctx context.Context, id string, until time.Time, cause string, err *error) bool {
	tag, e := w.Pool.Exec(ctx, `update jobs set run_at = greatest(least($2, now() + interval '25 hours'), now() + interval '1 second'), attempts = attempts - 1,
		last_error = left($3, 2000), locked_until = null where id = $1 and created_at > now() - interval '72 hours'`, id, until, cause)
	*err = e
	return e != nil || tag.RowsAffected() == 1
}

// park tells kind's Parked hook, if it has one, that the queue gave up.
func (w *Worker) park(ctx context.Context, kind string, payload []byte, cause string) {
	if hook := w.Parked[kind]; hook != nil {
		hook(ctx, payload, cause)
	}
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
// for a day, old abandoned checkouts, and parked jobs after 30 days, a batch
// at a time, and queues a settle for every abandoned order. Every step runs; the errors come back joined.
func Sweep(ctx context.Context, pool *pgxpool.Pool) error {
	var errs []error
	for _, q := range []string{
		`delete from sessions where id in (select id from sessions where expires_at < now() or created_at < now() - interval '90 days' limit $1)`,
		`delete from webauthn_challenges where id in (select id from webauthn_challenges where expires_at < now() limit $1)`,
		`delete from email_tokens where token_hash in (select token_hash from email_tokens where expires_at < now() limit $1)`,
		`delete from idempotency_keys where (scope, key) in (select scope, key from idempotency_keys where expires_at < now() limit $1)`,
		`delete from jobs where id in (select id from jobs where run_at = 'infinity' and created_at < now() - interval '30 days' limit $1)`,
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
	for _, fn := range []string{`select purge_expired_checkouts($1)`, `select purge_expired_link_states($1)`, `select queue_abandoned_orders($1)`} {
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
		return Deliver(ctx, m, mailer.Message{To: p.To, Subject: p.Subject, Text: p.Text})
	}
}

// now is Deliver's clock; tests pin it.
var now = time.Now

// Deliver is the one way a job sends email. Inside a job, a message without
// a key gets "ts-job-<job id>", so every retry of the job is the same email
// to the provider. A provider's Permanent answer parks the job at once; a
// DailyQuota answer waits for the next 00:05 UTC without spending an attempt.
func Deliver(ctx context.Context, m mailer.Mailer, msg mailer.Message) error {
	if id := ID(ctx); id != "" && msg.IdempotencyKey == "" {
		msg.IdempotencyKey = "ts-job-" + id
	}
	err := m.Send(ctx, msg)
	if e, ok := errors.AsType[*mailer.Error](err); ok {
		switch e.Kind {
		case mailer.Permanent:
			return Permanent(err)
		case mailer.DailyQuota:
			t := now().UTC()
			next := time.Date(t.Year(), t.Month(), t.Day(), 0, 5, 0, 0, time.UTC)
			if !next.After(t) {
				next = next.AddDate(0, 0, 1)
			}
			return Delay(next, err)
		}
	}
	return err
}
