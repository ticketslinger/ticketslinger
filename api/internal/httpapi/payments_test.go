package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// newPaymentsHarness is newHarness with a payment provider (nil for none)
// and the dashboard origin the onboarding links point back to.
func newPaymentsHarness(t *testing.T, p plugin.PaymentProvider) *harness {
	pool := testdb.New(t)
	m := &mailer.Memory{}
	svc := auth.NewService(pool, m, auth.Options{RPID: "localhost", Origin: "http://localhost:5173", SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	srv := startServer(t, httpapi.New(httpapi.Deps{Pool: pool, Auth: svc, Payments: p, Config: config.Config{DashboardOrigin: "http://localhost:5173"}}))
	return &harness{t, srv, m, pool, clientFor(t, srv.URL)}
}

// chargesEnabled marks slug as having finished Stripe onboarding, so its paid
// tickets can go on sale.
func (h *harness) chargesEnabled(slug string) {
	h.t.Helper()
	if _, err := testdb.Owner(h.t, h.pool).Exec(context.Background(),
		`update organizers set stripe_account_id = 'acct_' || replace(slug::text, '-', ''), stripe_link_method = 'created', stripe_charges_enabled = true where slug = $1`, slug); err != nil {
		h.t.Fatal(err)
	}
}

const paymentsURL = "/v1/organizers/acme/payments"

func TestConnectStripe(t *testing.T) {
	fake := payments.NewFake()
	h := newPaymentsHarness(t, fake)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")

	st := h.must(h.do(owner, "GET", paymentsURL, ""), 200)
	if st["provider"] != "test" || st["connected"] != false || st["charges_enabled"] != false || st["details_submitted"] != false {
		t.Fatalf("before connecting: %v", st)
	}
	if res := h.do(owner, "POST", paymentsURL+"/refresh", ""); res.StatusCode != 409 || h.errorCode(res) != "not_connected" {
		t.Fatalf("refresh before connecting: %d %q", res.StatusCode, h.errorCode(res))
	}

	link := h.must(h.do(owner, "POST", paymentsURL+"/connect", ""), 200)
	if link["url"] != "http://localhost:5173/o/acme/payments?return=1" {
		t.Fatalf("connect: %v", link)
	}
	h.must(h.do(owner, "POST", paymentsURL+"/connect", ""), 200)
	if n := fake.Accounts(); n != 1 {
		t.Fatalf("a second connect made %d accounts, want 1", n)
	}
	st = h.must(h.do(owner, "GET", paymentsURL, ""), 200)
	if st["connected"] != true || st["charges_enabled"] != false {
		t.Fatalf("connected, not refreshed: %v", st)
	}
	st = h.must(h.do(owner, "POST", paymentsURL+"/refresh", ""), 200)
	if st["connected"] != true || st["charges_enabled"] != true || st["details_submitted"] != true {
		t.Fatalf("refresh: %v", st)
	}
	if st = h.must(h.do(owner, "GET", paymentsURL, ""), 200); st["charges_enabled"] != true {
		t.Fatalf("refresh didn't stick: %v", st)
	}

	var n int
	if err := testdb.Owner(t, h.pool).QueryRow(context.Background(), `select count(*) from audit_log a join organizers o on o.id = a.organizer_id
		where a.action = 'organizer.stripe_connected' and a.entity_type = 'organizer' and a.after->>'stripe_account_id' = o.stripe_account_id
		and starts_with(o.stripe_account_id, 'acct_fake')`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows %d %v, want 1", n, err)
	}

	// Stripe being down is a 503 the dashboard can retry.
	fake.FailNext(errors.New("stripe: account: connection refused"))
	if res := h.do(owner, "POST", paymentsURL+"/refresh", ""); res.StatusCode != 503 || h.errorCode(res) != "payments_unavailable" {
		t.Fatalf("refresh while Stripe is down: %d %q", res.StatusCode, h.errorCode(res))
	}
	fake.FailNext(errors.New("stripe: account link: connection refused"))
	if res := h.do(owner, "POST", paymentsURL+"/connect", ""); res.StatusCode != 503 || h.errorCode(res) != "payments_unavailable" {
		t.Fatalf("connect while Stripe is down: %d %q", res.StatusCode, h.errorCode(res))
	}
}

func TestConcurrentConnectsMakeOneAccount(t *testing.T) {
	fake := payments.NewFake()
	h := newPaymentsHarness(t, fake)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	// Hold the organizer row so every request reaches the database before any
	// can finish; without the handler's row lock they'd all see no account.
	ctx := context.Background()
	root := testdb.Owner(t, h.pool)
	hold, err := root.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `select 1 from organizers where slug = 'acme' for update`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Go(func() {
			req, _ := http.NewRequest("POST", h.srv.URL+paymentsURL+"/connect", nil)
			req.Header.Set("Authorization", "Bearer "+owner)
			res, err := h.client.Do(req)
			if err != nil {
				return
			}
			codes[i] = res.StatusCode
			_ = res.Body.Close()
		})
	}
	// pg_locks shows other roles' waits; pg_stat_activity hides them.
	for waiting, start := 0, time.Now(); waiting < len(codes); {
		if err := root.QueryRow(ctx, `select count(distinct pid) from pg_locks where not granted
			and pid in (select pid from pg_stat_activity where datname = current_database())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("%d requests waiting on the organizer row, want %d", waiting, len(codes))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for _, c := range codes {
		if c != 200 {
			t.Fatalf("statuses %v", codes)
		}
	}
	if n := fake.Accounts(); n != 1 {
		t.Fatalf("%d concurrent connects made %d accounts, want 1", len(codes), n)
	}
	// Stripe's idempotency key hides a repeated create; the audit log doesn't.
	var created int
	if err := root.QueryRow(ctx, `select count(*) from audit_log where action = 'organizer.stripe_connected'`).Scan(&created); err != nil || created != 1 {
		t.Fatalf("%d connects recorded %d accounts (%v), want 1", len(codes), created, err)
	}
}

func TestConnectStripeAccess(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	staff := h.signupWithMFA("staff@example.com")
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(), `insert into memberships (organizer_id, user_id, role)
		select o.id, u.id, 'checkin' from organizers o, users u where o.slug = 'acme' and u.email = 'staff@example.com'`); err != nil {
		t.Fatal(err)
	}
	h.must(h.do(staff, "GET", paymentsURL, ""), 200)
	for _, op := range []string{"/connect", "/refresh"} {
		if res := h.do(staff, "POST", paymentsURL+op, ""); res.StatusCode != 403 || h.errorCode(res) != "forbidden_role" {
			t.Fatalf("checkin %s: %d %q", op, res.StatusCode, h.errorCode(res))
		}
	}
	_, write := h.createKey(owner, "acme", "write")
	_, read := h.createKey(owner, "acme", "read")
	if res := h.do(write, "POST", paymentsURL+"/connect", ""); res.StatusCode != 403 || h.errorCode(res) != "session_required" {
		t.Fatalf("key connect: %d %q", res.StatusCode, h.errorCode(res))
	}
	h.must(h.do(read, "GET", paymentsURL, ""), 200)
	if res := h.do(read, "POST", paymentsURL+"/refresh", ""); res.StatusCode != 403 || h.errorCode(res) != "scope_read_only" {
		t.Fatalf("read key refresh: %d %q", res.StatusCode, h.errorCode(res))
	}
	h.must(h.do(owner, "POST", paymentsURL+"/connect", ""), 200)
	if st := h.must(h.do(write, "POST", paymentsURL+"/refresh", ""), 200); st["charges_enabled"] != true {
		t.Fatalf("write key refresh: %v", st)
	}
	plain := h.signupVerified("plain@example.com")
	h.createOrganizer(plain, "plain")
	if res := h.do(plain, "POST", "/v1/organizers/plain/payments/connect", ""); res.StatusCode != 403 || h.errorCode(res) != "mfa_required" {
		t.Fatalf("owner without mfa: %d %q", res.StatusCode, h.errorCode(res))
	}
}

func TestPaymentsNotConfigured(t *testing.T) {
	h := newPaymentsHarness(t, nil)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	if st := h.must(h.do(owner, "GET", paymentsURL, ""), 200); st["provider"] != "none" || st["connected"] != false {
		t.Fatalf("status: %v", st)
	}
	for _, op := range []string{"/connect", "/refresh"} {
		if res := h.do(owner, "POST", paymentsURL+op, ""); res.StatusCode != 503 || h.errorCode(res) != "payments_not_configured" {
			t.Fatalf("%s: %d %q", op, res.StatusCode, h.errorCode(res))
		}
	}
	// Someone else's organizer is still a 404, not a hint about payments.
	stranger := h.signupWithMFA("stranger@example.com")
	if res := h.do(stranger, "POST", paymentsURL+"/connect", ""); res.StatusCode != 404 {
		t.Fatalf("stranger: %d", res.StatusCode)
	}
}

// An onboarding link is a short-lived credential: never kept for replay.
func TestOnboardingLinksAreNotKept(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	for range 2 {
		if res, body := h.doKeyed(owner, "POST", paymentsURL+"/connect", "connect-1", ""); res.StatusCode != 200 || res.Header.Get("Idempotent-Replayed") != "" {
			t.Fatalf("connect: %d replayed=%q %s", res.StatusCode, res.Header.Get("Idempotent-Replayed"), body)
		}
	}
	var rows int
	if err := h.pool.QueryRow(context.Background(), `select count(*) from idempotency_keys`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("idempotency rows = %d (%v), want 0", rows, err)
	}
}

// blockingLink holds the first onboarding link until release closes, so a
// test can act while a connect sits on the organizer row mid-Stripe-call.
type blockingLink struct {
	*payments.Fake
	entered, release chan struct{}
	once             sync.Once
}

func (b *blockingLink) OnboardingLink(ctx context.Context, account, refreshURL, returnURL string) (string, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.Fake.OnboardingLink(ctx, account, refreshURL, returnURL)
}

// A connect waiting on Stripe must not hold up the organizer's other writes:
// their foreign keys take FOR KEY SHARE on the organizer row.
func TestConnectDoesNotBlockOtherWrites(t *testing.T) {
	p := &blockingLink{Fake: payments.NewFake(), entered: make(chan struct{}), release: make(chan struct{})}
	h := newPaymentsHarness(t, p)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	connected := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", h.srv.URL+paymentsURL+"/connect", nil)
		req.Header.Set("Authorization", "Bearer "+owner)
		res, err := h.client.Do(req)
		if err != nil {
			connected <- 0
			return
		}
		_ = res.Body.Close()
		connected <- res.StatusCode
	}()
	<-p.entered
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", h.srv.URL+"/v1/organizers/acme/events", strings.NewReader(eventBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+owner)
	res, err := h.client.Do(req)
	close(p.release)
	if err != nil {
		t.Fatalf("creating an event while a connect waits on Stripe: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatalf("create event: %d", res.StatusCode)
	}
	if code := <-connected; code != 200 {
		t.Fatalf("connect: %d", code)
	}
}
