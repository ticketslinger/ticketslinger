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

type Worker struct {
	Pool     *pgxpool.Pool
	Handlers map[string]Handler
	Poll     time.Duration // default 1s
}

const claim = `update jobs set locked_until = now() + interval '5 minutes', attempts = attempts + 1
	where id = (select id from jobs where run_at <= now() and (locked_until is null or locked_until < now())
	            order by run_at, id limit 1 for update skip locked)
	returning id, kind, payload, attempts`

const fail = `update jobs set run_at = case when attempts >= 10 then 'infinity'::timestamptz
	else now() + least(interval '1 minute' * power(2, attempts), interval '6 hours') end,
	last_error = left($2, 2000), locked_until = null where id = $1`

// Run claims and runs due jobs until ctx ends. A failed job is retried with
// backoff min(2^attempts minutes, 6h); after 10 attempts it stays with
// run_at = 'infinity' and last_error set, for the organizer to see later.
// It also calls Sweep every minute.
func (w *Worker) Run(ctx context.Context) {
	poll := w.Poll
	if poll <= 0 {
		poll = time.Second
	}
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()
	for ctx.Err() == nil {
		ran, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "jobs: claim", "err", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-sweep.C:
			if err := Sweep(ctx, w.Pool); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "jobs: sweep", "err", err)
			}
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
		return false, nil
	}
	if err != nil {
		return false, err
	}
	herr := w.handle(ctx, kind, payload)
	// A shutdown mid-job must still record the outcome.
	rec := context.WithoutCancel(ctx)
	if herr != nil {
		slog.WarnContext(ctx, "jobs: failed", "kind", kind, "attempt", attempts, "err", herr)
		_, err = w.Pool.Exec(rec, fail, id, herr.Error())
	} else {
		_, err = w.Pool.Exec(rec, `delete from jobs where id = $1`, id)
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
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return h(ctx, payload)
}

// Sweep deletes expired sessions, webauthn challenges, email tokens and
// idempotency keys, and old abandoned checkouts.
func Sweep(ctx context.Context, pool *pgxpool.Pool) error {
	for _, q := range []string{
		`delete from sessions where expires_at < now() or created_at < now() - interval '90 days'`,
		`delete from webauthn_challenges where expires_at < now()`,
		`delete from email_tokens where expires_at < now()`,
		`select purge_expired_checkouts()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
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
