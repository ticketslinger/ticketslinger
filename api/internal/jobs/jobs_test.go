package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func enqueue(t *testing.T, pool *pgxpool.Pool, kind string, payload any) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return jobs.Enqueue(context.Background(), tx, kind, payload)
	}); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string) (n int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJobRunsOnceAndIsDeleted(t *testing.T) {
	pool := testdb.New(t)
	var got []string
	w := &jobs.Worker{Pool: pool, Handlers: map[string]jobs.Handler{"ping": func(_ context.Context, p []byte) error {
		got = append(got, string(p))
		return nil
	}}}
	enqueue(t, pool, "ping", map[string]int{"n": 1})
	ran, err := w.RunOnce(context.Background())
	if err != nil || !ran {
		t.Fatalf("RunOnce = %v, %v", ran, err)
	}
	if len(got) != 1 || got[0] != `{"n": 1}` {
		t.Fatalf("handler got %q", got)
	}
	if n := count(t, pool, `select count(*) from jobs`); n != 0 {
		t.Fatalf("%d jobs left", n)
	}
	if ran, _ := w.RunOnce(context.Background()); ran {
		t.Fatal("second RunOnce ran something")
	}
}

func TestRolledBackEnqueueLeavesNothing(t *testing.T) {
	pool := testdb.New(t)
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := jobs.Enqueue(context.Background(), tx, "ping", 1); err != nil {
			return err
		}
		return errors.New("nope")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	if n := count(t, pool, `select count(*) from jobs`); n != 0 {
		t.Fatalf("%d jobs after rollback", n)
	}
}

func TestFailedJobBacksOff(t *testing.T) {
	pool := testdb.New(t)
	w := &jobs.Worker{Pool: pool, Handlers: map[string]jobs.Handler{"bad": func(context.Context, []byte) error { return errors.New("boom") }}}
	enqueue(t, pool, "bad", 1)
	if ran, err := w.RunOnce(context.Background()); err != nil || !ran {
		t.Fatalf("RunOnce = %v, %v", ran, err)
	}
	var attempts int
	var lastErr *string
	var locked bool
	var secs float64
	if err := pool.QueryRow(context.Background(), `select attempts, last_error, locked_until is not null,
		extract(epoch from run_at - now()) from jobs`).Scan(&attempts, &lastErr, &locked, &secs); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr == nil || *lastErr != "boom" || locked || secs < 110 || secs > 121 {
		t.Fatalf("attempts=%d last_error=%v locked=%v run_at in %.0fs, want 1, boom, false, ~120s", attempts, lastErr, locked, secs)
	}
	if ran, _ := w.RunOnce(context.Background()); ran {
		t.Fatal("job ran again before its backoff")
	}

	if _, err := pool.Exec(context.Background(), `update jobs set attempts = 9, run_at = now()`); err != nil {
		t.Fatal(err)
	}
	if ran, _ := w.RunOnce(context.Background()); !ran {
		t.Fatal("10th attempt did not run")
	}
	if n := count(t, pool, `select count(*) from jobs where run_at = 'infinity' and attempts = 10 and last_error = 'boom'`); n != 1 {
		t.Fatal("job was not parked after the 10th failure")
	}
}

func TestUnknownKindFails(t *testing.T) {
	pool := testdb.New(t)
	w := &jobs.Worker{Pool: pool, Handlers: map[string]jobs.Handler{}}
	enqueue(t, pool, "mystery", 1)
	if ran, err := w.RunOnce(context.Background()); err != nil || !ran {
		t.Fatalf("RunOnce = %v, %v", ran, err)
	}
	if n := count(t, pool, `select count(*) from jobs where last_error = 'no handler for kind mystery' and attempts = 1`); n != 1 {
		t.Fatal("unknown kind was not recorded as a failure")
	}
}

func TestTwoWorkersNeverRunTheSameJob(t *testing.T) {
	pool := testdb.New(t)
	var mu sync.Mutex
	seen := map[int]int{}
	var total atomic.Int64
	h := map[string]jobs.Handler{"n": func(_ context.Context, p []byte) error {
		var v struct{ N int }
		if err := json.Unmarshal(p, &v); err != nil {
			return err
		}
		mu.Lock()
		seen[v.N]++
		mu.Unlock()
		total.Add(1)
		return nil
	}}
	for i := 0; i < 50; i++ {
		enqueue(t, pool, "n", map[string]int{"N": i})
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		w := &jobs.Worker{Pool: pool, Handlers: h}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ran, err := w.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if !ran {
					return
				}
			}
		}()
	}
	wg.Wait()
	if total.Load() != 50 || len(seen) != 50 {
		t.Fatalf("ran %d times over %d payloads, want 50 and 50", total.Load(), len(seen))
	}
	for n, c := range seen {
		if c != 1 {
			t.Fatalf("payload %d ran %d times", n, c)
		}
	}
}

func TestRunStopsWhenContextEnds(t *testing.T) {
	pool := testdb.New(t)
	w := &jobs.Worker{Pool: pool, Poll: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSweepRemovesOnlyExpired(t *testing.T) {
	pool := testdb.New(t)
	owner := testdb.Owner(t, pool)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	var user, org, event string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(owner.QueryRow(ctx, `insert into users(email, name) values ('a@example.com', 'A') returning id`).Scan(&user))
	must(owner.QueryRow(ctx, `insert into organizers(name, slug) values ('Acme', 'acme') returning id`).Scan(&org))
	must(owner.QueryRow(ctx, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, 'gala', 'Gala', 'America/New_York', now() + interval '6 days', now() + interval '7 days', 'live') returning id`, org).Scan(&event))

	sess := `insert into sessions(user_id, token_hash, mfa, expires_at, created_at) values ($1, $2, true, now() + $3::interval, now() - $4::interval)`
	exec(sess, user, []byte("live"), "1 hour", "1 day")
	exec(sess, user, []byte("expired"), "-1 minute", "1 day")
	exec(sess, user, []byte("ancient"), "1 hour", "91 days")
	exec(`insert into email_tokens(token_hash, user_id, purpose, expires_at) values ('\x01', $1, 'verify', now() - interval '1 minute'), ('\x02', $1, 'verify', now() + interval '1 hour')`, user)
	exec(`insert into webauthn_challenges(user_id, kind, data, expires_at) values ($1, 'login', '{}', now() - interval '1 minute'), ($1, 'login', '{}', now() + interval '1 minute')`, user)

	co := `insert into checkouts(organizer_id, event_id, token_hash, expires_at) values ($1, $2, $3, now() - interval '8 days') returning id`
	var gone, kept, recent string
	must(owner.QueryRow(ctx, co, org, event, make([]byte, 32)).Scan(&gone))
	b := make([]byte, 32)
	b[0] = 1
	must(owner.QueryRow(ctx, co, org, event, b).Scan(&kept))
	exec(`insert into orders(organizer_id, event_id, checkout_id, code, email, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents, fee_mode, quote, details)
		values ($1, $2, $3, 'TS-AAAAA', 'b@example.com', 1000, 0, 0, 0, 1000, 'pass', '{}', '{}')`, org, event, kept)
	b[0] = 2
	must(owner.QueryRow(ctx, `insert into checkouts(organizer_id, event_id, token_hash, expires_at) values ($1, $2, $3, now() - interval '1 day') returning id`, org, event, b).Scan(&recent))

	if err := jobs.Sweep(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for sql, want := range map[string]int{
		`select count(*) from sessions`:                            1,
		`select count(*) from email_tokens`:                        1,
		`select count(*) from webauthn_challenges`:                 1,
		`select count(*) from checkouts`:                           2,
		`select count(*) from checkouts where id = '` + gone + `'`: 0,
	} {
		if got := count(t, owner, sql); got != want {
			t.Errorf("%s = %d, want %d", sql, got, want)
		}
	}
	if err := jobs.Sweep(ctx, pool); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
}

func TestSendEmailHandler(t *testing.T) {
	m := &mailer.Memory{}
	p, _ := json.Marshal(map[string]string{"to": "a@example.com", "subject": "Hi", "text": "Hello"})
	if err := jobs.SendEmail(m)(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(m.Sent) != 1 || m.Sent[0] != (mailer.Message{To: "a@example.com", Subject: "Hi", Text: "Hello"}) {
		t.Fatalf("sent %+v", m.Sent)
	}
	if err := jobs.SendEmail(m)(context.Background(), []byte(`{`)); err == nil {
		t.Fatal("bad payload accepted")
	}
}
