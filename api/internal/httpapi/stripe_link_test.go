package httpapi_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

const linkURL = "/v1/organizers/acme/payments/link"

// startLink starts a link for slug and returns the state and code the fake
// sends back to the callback.
func (h *harness) startLink(tok, slug string) (state, code string) {
	h.t.Helper()
	b := h.must(h.do(tok, "POST", "/v1/organizers/"+slug+"/payments/link", ""), 200)
	u, err := url.Parse(b["url"].(string))
	if err != nil || u.Path != "/stripe/oauth/callback" {
		h.t.Fatalf("link url %v %v", b["url"], err)
	}
	return u.Query().Get("state"), u.Query().Get("code")
}

func (h *harness) completeLink(tok, state, code string) *http.Response {
	h.t.Helper()
	return h.do(tok, "POST", "/v1/stripe/link/complete", `{"state":"`+state+`","code":"`+code+`"}`)
}

// otherSession signs email in a second time, with a second factor, straight
// in the database: the same person on another device.
func (h *harness) otherSession(email string) string {
	h.t.Helper()
	tok := rand.Text()
	sum := sha256.Sum256([]byte(tok))
	if _, err := testdb.Owner(h.t, h.pool).Exec(context.Background(), `insert into sessions (user_id, token_hash, mfa, expires_at)
		select id, $2, true, now() + interval '1 hour' from users where email = $1`, email, sum[:]); err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func (h *harness) member(email, slug, role string) {
	h.t.Helper()
	if _, err := testdb.Owner(h.t, h.pool).Exec(context.Background(), `insert into memberships (organizer_id, user_id, role)
		select o.id, u.id, $3 from organizers o, users u where o.slug = $2 and u.email = $1`, email, slug, role); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) count(q string, args ...any) int {
	h.t.Helper()
	var n int
	if err := testdb.Owner(h.t, h.pool).QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestLinkRoundTrip(t *testing.T) {
	fake := payments.NewFake()
	h := newPaymentsHarness(t, fake)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	if st := h.must(h.do(owner, "GET", paymentsURL, ""), 200); st["can_link_existing"] != true || st["link_method"] != nil || st["disconnected"] != false {
		t.Fatalf("before: %v", st)
	}
	if b := h.must(h.do(owner, "POST", linkURL, ""), 200); !strings.HasPrefix(b["url"].(string), "http://localhost:5173/stripe/oauth/callback?") {
		t.Fatalf("not the registered callback: %v", b["url"])
	}
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(), `delete from stripe_link_states`); err != nil {
		t.Fatal(err)
	}
	state, code := h.startLink(owner, "acme")
	if len(state) != 43 || code != "fake_code_2" {
		t.Fatalf("state %q code %q", state, code)
	}
	// Only the state's hash is kept.
	if n := h.count(`select count(*) from stripe_link_states where state_hash = sha256($1::text::bytea)`, state); n != 1 {
		t.Fatalf("state rows %d", n)
	}
	if n := h.count(`select count(*) from stripe_link_states where position($1::text::bytea in state_hash) > 0`, state); n != 0 {
		t.Fatal("the state itself is stored")
	}
	b := h.must(h.completeLink(owner, state, code), 200)
	st := b["status"].(map[string]any)
	if b["organizer"] != "acme" || st["connected"] != true || st["charges_enabled"] != true || st["link_method"] != "linked" {
		t.Fatalf("complete: %v", b)
	}
	if n := h.count(`select count(*) from organizers where slug = 'acme' and stripe_account_id = 'acct_fakelink2' and stripe_link_method = 'linked'`); n != 1 {
		t.Fatal("acme isn't linked to acct_fakelink2")
	}
	if n := h.count(`select count(*) from audit_log where action = 'organizer.stripe_linked' and after->>'stripe_account_id' = 'acct_fakelink2'
		and after->>'via' = 'oauth' and actor_user_id = (select id from users where email = 'owner@example.com')`); n != 1 {
		t.Fatalf("audit rows %d, want 1", n)
	}
	// Nothing anywhere holds an OAuth token: the fake has none to give, and
	// the API never asked for one to keep (see payments.TestCompleteLinkReturnsTheAccount).
}

func TestLinkStateIsSingleUse(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	state, code := h.startLink(owner, "acme")
	h.must(h.completeLink(owner, state, code), 200)
	if res := h.completeLink(owner, state, code); res.StatusCode != 400 || h.errorCode(res) != "invalid_state" {
		t.Fatalf("second complete: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.completeLink(owner, "no-such-state-00000000000000000000000000000", code); res.StatusCode != 400 || h.errorCode(res) != "invalid_state" {
		t.Fatalf("unknown state: %d %q", res.StatusCode, h.errorCode(res))
	}
}

// Two tabs racing with one state: exactly one links.
func TestLinkStateRaceLinksOnce(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	state, code := h.startLink(owner, "acme")
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Go(func() {
			req, _ := http.NewRequest("POST", h.srv.URL+"/v1/stripe/link/complete", strings.NewReader(`{"state":"`+state+`","code":"`+code+`"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+owner)
			if res, err := h.client.Do(req); err == nil {
				codes[i] = res.StatusCode
				_ = res.Body.Close()
			}
		})
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{200, 400, 400, 400}) {
		t.Fatalf("statuses %v, want one 200", codes)
	}
	if n := h.count(`select count(*) from audit_log where action = 'organizer.stripe_linked'`); n != 1 {
		t.Fatalf("%d links recorded", n)
	}
}

func TestLinkStateExpires(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	state, code := h.startLink(owner, "acme")
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(), `update stripe_link_states set expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if res := h.completeLink(owner, state, code); res.StatusCode != 400 || h.errorCode(res) != "invalid_state" {
		t.Fatalf("expired: %d %q", res.StatusCode, h.errorCode(res))
	}
	if n := h.count(`select count(*) from organizers where stripe_account_id is not null`); n != 0 {
		t.Fatal("an expired state linked an account")
	}
}

// The state is the session's own: the same person elsewhere, another admin
// and a stranger all get the same 400, and none of them uses it up.
func TestLinkStateBelongsToItsSession(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	admin := h.signupWithMFA("admin@example.com")
	h.member("admin@example.com", "acme", "admin")
	stranger := h.signupWithMFA("stranger@example.com")
	state, code := h.startLink(owner, "acme")
	for name, tok := range map[string]string{"other session": h.otherSession("owner@example.com"), "other admin": admin, "stranger": stranger} {
		if res := h.completeLink(tok, state, code); res.StatusCode != 400 || h.errorCode(res) != "invalid_state" {
			t.Errorf("%s: %d %q, want 400 invalid_state", name, res.StatusCode, h.errorCode(res))
		}
	}
	h.must(h.completeLink(owner, state, code), 200)
}

func TestLinkNeedsOwnerOrAdminWithMFA(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	staff := h.signupWithMFA("staff@example.com")
	h.member("staff@example.com", "acme", "checkin")
	for _, op := range []string{"/link", "/disconnect"} {
		if res := h.do(staff, "POST", paymentsURL+op, ""); res.StatusCode != 403 || h.errorCode(res) != "forbidden_role" {
			t.Errorf("checkin %s: %d %q", op, res.StatusCode, h.errorCode(res))
		}
	}
	_, key := h.createKey(owner, "acme", "write")
	for _, path := range []string{paymentsURL + "/link", paymentsURL + "/disconnect", "/v1/stripe/link/complete"} {
		if res := h.do(key, "POST", path, `{"state":"s","code":"c"}`); res.StatusCode != 403 || h.errorCode(res) != "session_required" {
			t.Errorf("key %s: %d %q", path, res.StatusCode, h.errorCode(res))
		}
	}
	plain := h.signupVerified("plain@example.com")
	h.createOrganizer(plain, "plain")
	if res := h.do(plain, "POST", "/v1/organizers/plain/payments/link", ""); res.StatusCode != 403 || h.errorCode(res) != "mfa_required" {
		t.Fatalf("owner without mfa: %d %q", res.StatusCode, h.errorCode(res))
	}
	// A state started with MFA, finished by the same session after it lost
	// MFA, or by someone demoted since: refused as any other write would be.
	state, code := h.startLink(owner, "acme")
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(), `update memberships set "role" = 'checkin'
		where user_id = (select id from users where email = 'owner@example.com')`); err != nil {
		t.Fatal(err)
	}
	if res := h.completeLink(owner, state, code); res.StatusCode != 403 || h.errorCode(res) != "forbidden_role" {
		t.Fatalf("demoted: %d %q", res.StatusCode, h.errorCode(res))
	}
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(), `update memberships set "role" = 'owner'
		where user_id = (select id from users where email = 'owner@example.com');
		update sessions set mfa = false where user_id = (select id from users where email = 'owner@example.com')`); err != nil {
		t.Fatal(err)
	}
	if res := h.completeLink(owner, state, code); res.StatusCode != 403 || h.errorCode(res) != "mfa_required" {
		t.Fatalf("no mfa: %d %q", res.StatusCode, h.errorCode(res))
	}
}

func TestAccountInUseIsRefused(t *testing.T) {
	fake := payments.NewFake()
	h := newPaymentsHarness(t, fake)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	h.createOrganizer(owner, "beta")
	state, code := h.startLink(owner, "acme")
	h.must(h.completeLink(owner, state, code), 200)
	fake.LinkReturns("acct_fakelink1")
	state, code = h.startLink(owner, "beta")
	if res := h.completeLink(owner, state, code); res.StatusCode != 409 || h.errorCode(res) != "account_in_use" {
		t.Fatalf("second organizer: %d %q", res.StatusCode, h.errorCode(res))
	}
	if n := h.count(`select count(*) from organizers where slug = 'beta' and stripe_account_id is null`); n != 1 {
		t.Fatal("beta got an account")
	}
	if len(fake.Unlinked()) != 0 {
		t.Fatalf("unlinked %v: acme's sales would stop", fake.Unlinked())
	}
	// The refusal still used the state up.
	if res := h.completeLink(owner, state, code); res.StatusCode != 400 || h.errorCode(res) != "invalid_state" {
		t.Fatalf("state after refusal: %d %q", res.StatusCode, h.errorCode(res))
	}
}

func TestAlreadyConnectedMustDisconnectFirst(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	h.must(h.do(owner, "POST", paymentsURL+"/connect", ""), 200)
	if res := h.do(owner, "POST", linkURL, ""); res.StatusCode != 409 || h.errorCode(res) != "already_connected" {
		t.Fatalf("link while connected: %d %q", res.StatusCode, h.errorCode(res))
	}
	// Connected some other way after the link started: refused at the end too.
	h2 := newPaymentsHarness(t, payments.NewFake())
	owner2 := h2.signupWithMFA("owner@example.com")
	h2.createOrganizer(owner2, "acme")
	state, code := h2.startLink(owner2, "acme")
	h2.must(h2.do(owner2, "POST", paymentsURL+"/connect", ""), 200)
	if res := h2.completeLink(owner2, state, code); res.StatusCode != 409 || h2.errorCode(res) != "already_connected" {
		t.Fatalf("complete after connecting: %d %q", res.StatusCode, h2.errorCode(res))
	}
	// And a linked account can't be swapped for a created one without disconnecting.
	h3 := newPaymentsHarness(t, payments.NewFake())
	owner3 := h3.signupWithMFA("owner@example.com")
	h3.createOrganizer(owner3, "acme")
	state, code = h3.startLink(owner3, "acme")
	h3.must(h3.completeLink(owner3, state, code), 200)
	if res := h3.do(owner3, "POST", paymentsURL+"/connect", ""); res.StatusCode != 409 || h3.errorCode(res) != "already_connected" {
		t.Fatalf("create while linked: %d %q", res.StatusCode, h3.errorCode(res))
	}
}

// openPaidCheckout opens a checkout for one of acme's paid tickets.
func openPaidCheckout(t *testing.T, s shop) string {
	t.Helper()
	tok := openPaidCheckoutAfter(t, s)
	if tok == "" {
		t.Fatal("payments_not_ready")
	}
	return tok
}

func (s shop) pay(token string) *http.Response {
	s.t.Helper()
	return s.send("", "", "POST", "/v1/checkouts/"+token+"/pay", `{"email":"buyer@example.com"}`)
}

func TestDisconnectCreatedAccountIsLocalOnly(t *testing.T) {
	fake := payments.NewFake()
	s := newShopWith(t, nil, fake)
	if _, err := testdb.Owner(t, s.pool).Exec(context.Background(), `update organizers set stripe_account_id = null, stripe_link_method = null where slug = 'acme'`); err != nil {
		t.Fatal(err)
	}
	s.must(s.do(s.owner, "POST", paymentsURL+"/connect", ""), 200)
	s.must(s.do(s.owner, "POST", paymentsURL+"/refresh", ""), 200)
	tok := openPaidCheckout(t, s)
	fake.FailNext(errors.New("stripe: deauthorize: connection refused"))
	st := s.must(s.do(s.owner, "POST", paymentsURL+"/disconnect", ""), 200)
	if st["disconnected"] != true || st["connected"] != false || st["charges_enabled"] != false || st["link_method"] != "created" {
		t.Fatalf("disconnect: %v", st)
	}
	if len(fake.Unlinked()) != 0 {
		t.Fatalf("a created account was deauthorized: %v", fake.Unlinked())
	}
	fake.FailNext(nil)
	if n := s.count(`select count(*) from audit_log where action = 'organizer.stripe_disconnected' and after->>'via' = 'dashboard'`); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	if res := s.pay(tok); res.StatusCode != 409 || s.errorCode(res) != "payments_not_ready" {
		t.Fatalf("pay after disconnect: %d %q", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(s.owner, "POST", paymentsURL+"/refresh", ""); res.StatusCode != 409 || s.errorCode(res) != "not_connected" {
		t.Fatalf("refresh while disconnected: %d %q", res.StatusCode, s.errorCode(res))
	}
	if res := s.do(s.owner, "POST", paymentsURL+"/disconnect", ""); res.StatusCode != 409 || s.errorCode(res) != "not_connected" {
		t.Fatalf("second disconnect: %d %q", res.StatusCode, s.errorCode(res))
	}
	// Create again: the same account, switched back on, no new one made,
	// and live again from now, so an older deauthorize can't switch it off.
	var first time.Time
	if err := testdb.Owner(t, s.pool).QueryRow(context.Background(), `select stripe_connected_at from organizers where slug = 'acme'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	before := fake.Accounts()
	s.must(s.do(s.owner, "POST", paymentsURL+"/connect", ""), 200)
	var again time.Time
	if err := testdb.Owner(t, s.pool).QueryRow(context.Background(), `select stripe_connected_at from organizers where slug = 'acme'`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if !again.After(first) {
		t.Fatalf("stripe_connected_at %v after reconnecting, was %v: want it to move forward", again, first)
	}
	if st := s.must(s.do(s.owner, "POST", paymentsURL+"/refresh", ""), 200); st["connected"] != true || st["charges_enabled"] != true || st["disconnected"] != false {
		t.Fatalf("reconnect: %v", st)
	}
	if fake.Accounts() != before {
		t.Fatalf("reconnecting made a new account: %d, was %d", fake.Accounts(), before)
	}
}

func TestDisconnectLinkedAccountDeauthorizes(t *testing.T) {
	fake := payments.NewFake()
	s := newShopWith(t, nil, fake)
	if _, err := testdb.Owner(t, s.pool).Exec(context.Background(), `update organizers set stripe_account_id = null, stripe_link_method = null where slug = 'acme'`); err != nil {
		t.Fatal(err)
	}
	state, code := s.startLink(s.owner, "acme")
	s.must(s.completeLink(s.owner, state, code), 200)
	tok := openPaidCheckout(t, s)

	fake.FailNext(errors.New("stripe: deauthorize: connection refused"))
	if res := s.do(s.owner, "POST", paymentsURL+"/disconnect", ""); res.StatusCode != 503 || s.errorCode(res) != "payments_unavailable" {
		t.Fatalf("Stripe down: %d %q", res.StatusCode, s.errorCode(res))
	}
	if st := s.must(s.do(s.owner, "GET", paymentsURL, ""), 200); st["connected"] != true || st["charges_enabled"] != true {
		t.Fatalf("a failed disconnect changed things: %v", st)
	}
	// A buyer mid-payment holds it off.
	s.must(s.pay(tok), 200)
	if res := s.do(s.owner, "POST", paymentsURL+"/disconnect", ""); res.StatusCode != 409 || s.errorCode(res) != "payments_in_progress" {
		t.Fatalf("while paying: %d %q", res.StatusCode, s.errorCode(res))
	}
	if len(fake.Unlinked()) != 0 {
		t.Fatalf("unlinked while a buyer was paying: %v", fake.Unlinked())
	}
	if _, err := testdb.Owner(t, s.pool).Exec(context.Background(), `update checkouts set expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	st := s.must(s.do(s.owner, "POST", paymentsURL+"/disconnect", ""), 200)
	if st["disconnected"] != true || st["charges_enabled"] != false || st["link_method"] != "linked" {
		t.Fatalf("disconnect: %v", st)
	}
	if !slices.Equal(fake.Unlinked(), []string{"acct_fakelink1"}) {
		t.Fatalf("unlinked %v", fake.Unlinked())
	}
	if n := s.count(`select count(*) from audit_log where action = 'organizer.stripe_disconnected' and after->>'stripe_account_id' = 'acct_fakelink1'`); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	tok = openPaidCheckoutAfter(t, s)
	if tok != "" {
		t.Fatal("a checkout for paid tickets opened while disconnected")
	}
	// Linking the same account again works, here or for another organizer.
	fake.LinkReturns("acct_fakelink1")
	s.createOrganizer(s.owner, "beta")
	state, code = s.startLink(s.owner, "beta")
	s.must(s.completeLink(s.owner, state, code), 200)
	s.must(s.do(s.owner, "POST", "/v1/organizers/beta/payments/disconnect", ""), 200)
	state, code = s.startLink(s.owner, "acme")
	if st := s.must(s.completeLink(s.owner, state, code), 200)["status"].(map[string]any); st["connected"] != true || st["disconnected"] != false {
		t.Fatalf("relink: %v", st)
	}
}

// openPaidCheckoutAfter tries to open a paid checkout and returns its token,
// or "" when it's refused with payments_not_ready.
func openPaidCheckoutAfter(t *testing.T, s shop) string {
	t.Helper()
	res := s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, ""))
	if res.StatusCode == 409 && s.errorCode(res) == "payments_not_ready" {
		return ""
	}
	return s.must(res, 201)["token"].(string)
}

// stripeDouble stands in for api.stripe.com in the connect flow: the first
// account creates answer with status (if set), then accounts are made.
type stripeDouble struct {
	mu       sync.Mutex
	statuses []int    // answers for the next account creates, in order
	keys     []string // Idempotency-Key of each account create
}

func (d *stripeDouble) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Request-Id", "req_double")
	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.URL.Path {
	case "/v1/accounts":
		d.keys = append(d.keys, r.Header.Get("Idempotency-Key"))
		if len(d.statuses) > 0 {
			st := d.statuses[0]
			d.statuses = d.statuses[1:]
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"enable Accounts v1 support in the Dashboard"}}`))
			return
		}
		b, _ := os.ReadFile("../payments/testdata/account.json")
		_, _ = w.Write(b)
	case "/v1/account_links":
		b, _ := os.ReadFile("../payments/testdata/account_link.json")
		_, _ = w.Write(b)
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"no fixture"}}`))
	}
}

// Stripe replays a refusal under the same idempotency key for 24 hours. A
// definite refusal moves the key on, so the next try reaches Stripe afresh;
// a 5xx keeps it, since that create may have worked.
func TestRefusedCreateMovesTheKeyOn(t *testing.T) {
	double := &stripeDouble{statuses: []int{400, 500, 429}}
	srv := httptest.NewServer(double) // raw: stands in for Stripe, not our API
	t.Cleanup(srv.Close)
	b := stripe.NewBackendsWithConfig(&stripe.BackendConfig{URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull}})
	pool := testdb.New(t)
	m := &mailer.Memory{}
	svc := auth.NewService(pool, m, auth.Options{RPID: "localhost", Origin: "http://localhost:5173", SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	api := startServer(t, httpapi.New(httpapi.Deps{Pool: pool, Auth: svc, Payments: payments.NewStripe("sk_test_x", "pk_test_x", "", b),
		Config: config.Config{DashboardOrigin: "http://localhost:5173"}}))
	h := &harness{t, api, m, pool, clientFor(t, api.URL)}
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	var org string
	if err := testdb.Owner(t, pool).QueryRow(context.Background(), `select id::text from organizers where slug = 'acme'`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		status int
		code   string
	}{{503, "payments_refused"}, {503, "payments_unavailable"}, {503, "payments_unavailable"}, {200, ""}} {
		res := h.do(owner, "POST", paymentsURL+"/connect", "")
		if res.StatusCode != want.status || h.errorCode(res) != want.code {
			t.Fatalf("connect: %d %q, want %d %q", res.StatusCode, h.errorCode(res), want.status, want.code)
		}
	}
	// 400 refused under -0, then -1 throughout: the 500 and 429 may have
	// made the account, so the key that finally works is the same one.
	want := []string{"acct-" + org + "-0", "acct-" + org + "-1", "acct-" + org + "-1", "acct-" + org + "-1"}
	if !slices.Equal(double.keys, want) {
		t.Fatalf("keys %v, want %v", double.keys, want)
	}
	if n := h.count(`select stripe_create_attempt from organizers where slug = 'acme'`); n != 1 {
		t.Fatalf("attempt %d, want 1", n)
	}
	if n := h.count(`select count(*) from organizers where slug = 'acme' and stripe_account_id = 'acct_test1' and stripe_link_method = 'created'`); n != 1 {
		t.Fatal("acme isn't connected to the account")
	}
}

// linkedShop is acme with acct_fakelink1 linked and nothing else going on.
func linkedShop(t *testing.T) (shop, *payments.Fake) {
	t.Helper()
	fake := payments.NewFake()
	s := newShopWith(t, nil, fake)
	if _, err := testdb.Owner(t, s.pool).Exec(context.Background(), `update organizers set stripe_account_id = null, stripe_link_method = null where slug = 'acme'`); err != nil {
		t.Fatal(err)
	}
	state, code := s.startLink(s.owner, "acme")
	s.must(s.completeLink(s.owner, state, code), 200)
	return s, fake
}

var invalidClient = &plugin.ProviderError{Op: "stripe: deauthorize", Refused: true, Status: 400, Code: "invalid_client"}

// Stripe answers invalid_client to a deauthorize both when the account is
// already unlinked and when our client id is wrong, so a read of the account
// decides.
func TestDisconnectWhenStripeSaysInvalidClient(t *testing.T) {
	t.Run("no access: already unlinked", func(t *testing.T) {
		s, fake := linkedShop(t)
		fake.FailCall("Unlink", invalidClient)
		fake.FailCall("Account", &plugin.ProviderError{Op: "stripe: account", Refused: true, NoAccess: true, Status: 403, Code: "account_invalid"})
		if st := s.must(s.do(s.owner, "POST", paymentsURL+"/disconnect", ""), 200); st["disconnected"] != true {
			t.Fatalf("disconnect: %v", st)
		}
		if n := s.count(`select count(*) from audit_log where action = 'organizer.stripe_disconnected' and after->>'via' = 'dashboard'
			and after->>'note' = 'Stripe had already unlinked it'`); n != 1 {
			t.Fatalf("audit rows %d", n)
		}
	})
	t.Run("still has access", func(t *testing.T) {
		s, fake := linkedShop(t)
		fake.FailCall("Unlink", invalidClient)
		if res := s.do(s.owner, "POST", paymentsURL+"/disconnect", ""); res.StatusCode != 503 || s.errorCode(res) != "payments_refused" {
			t.Fatalf("probe works: %d %q", res.StatusCode, s.errorCode(res))
		}
		if st := s.must(s.do(s.owner, "GET", paymentsURL, ""), 200); st["connected"] != true {
			t.Fatalf("changed: %v", st)
		}
	})
	t.Run("probe unavailable", func(t *testing.T) {
		s, fake := linkedShop(t)
		fake.FailCall("Unlink", invalidClient)
		fake.FailCall("Account", &plugin.ProviderError{Op: "stripe: account", Cause: "timeout"})
		if res := s.do(s.owner, "POST", paymentsURL+"/disconnect", ""); res.StatusCode != 503 || s.errorCode(res) != "payments_unavailable" {
			t.Fatalf("probe down: %d %q", res.StatusCode, s.errorCode(res))
		}
		if st := s.must(s.do(s.owner, "GET", paymentsURL, ""), 200); st["connected"] != true {
			t.Fatalf("changed: %v", st)
		}
	})
	t.Run("another refusal is not probed", func(t *testing.T) {
		s, fake := linkedShop(t)
		fake.FailCall("Unlink", &plugin.ProviderError{Op: "stripe: deauthorize", Refused: true, Status: 400, Code: "invalid_request"})
		fake.FailCall("Account", &plugin.ProviderError{Op: "stripe: account", Refused: true, NoAccess: true, Status: 403})
		if res := s.do(s.owner, "POST", paymentsURL+"/disconnect", ""); res.StatusCode != 503 || s.errorCode(res) != "payments_refused" {
			t.Fatalf("invalid_request: %d %q", res.StatusCode, s.errorCode(res))
		}
	})
}

// A failed code exchange still uses the state up: start again.
func TestFailedExchangeBurnsTheState(t *testing.T) {
	fake := payments.NewFake()
	h := newPaymentsHarness(t, fake)
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	state, code := h.startLink(owner, "acme")
	fake.FailNext(errors.New("stripe: oauth token: connection reset"))
	if res := h.completeLink(owner, state, code); res.StatusCode != 503 || h.errorCode(res) != "payments_unavailable" {
		t.Fatalf("exchange down: %d %q", res.StatusCode, h.errorCode(res))
	}
	if res := h.completeLink(owner, state, code); res.StatusCode != 400 || h.errorCode(res) != "invalid_state" {
		t.Fatalf("retry: %d %q", res.StatusCode, h.errorCode(res))
	}
}

// After a linked account is disconnected, "Create a Stripe account" makes a
// new one, and the organizer is on a created account from then on.
func TestCreateAfterDisconnectingALinkedAccount(t *testing.T) {
	s, fake := linkedShop(t)
	s.must(s.do(s.owner, "POST", paymentsURL+"/disconnect", ""), 200)
	before := fake.Accounts()
	s.must(s.do(s.owner, "POST", paymentsURL+"/connect", ""), 200)
	if fake.Accounts() != before+1 {
		t.Fatalf("accounts %d, was %d: want a new one", fake.Accounts(), before)
	}
	if n := s.count(`select count(*) from organizers where slug = 'acme' and stripe_link_method = 'created' and starts_with(stripe_account_id, 'acct_fake')
		and stripe_account_id <> 'acct_fakelink1' and stripe_disconnected_at is null and stripe_connected_at is not null`); n != 1 {
		t.Fatal("acme isn't on a new created account")
	}
}

// suspendBySQL suspends slug as staff_act would, without a staff login.
func suspendBySQL(t *testing.T, h *harness, slug string) {
	t.Helper()
	if _, err := testdb.Owner(t, h.pool).Exec(context.Background(), `with
		o as (select id from organizers where slug = $1),
		u as (insert into users(email, name, email_verified_at) values ('staff@example.com', 'Staff', now()) returning id),
		st as (insert into staff(user_id, granted_by) select id, 'test' from u returning user_id),
		ss as (insert into staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, expires_at)
			select st.user_id, gen_random_uuid(), o.id, $1, 'Acme', 'Checking a takedown report', now() + interval '1 hour' from st, o returning id, user_id, organizer_id),
		a as (insert into staff_actions(staff_session_id, user_id, organizer_id, kind, reason)
			select id, user_id, organizer_id, 'suspend', 'Checking a takedown report' from ss returning id, organizer_id)
		insert into organizer_suspensions(organizer_id, staff_action_id) select organizer_id, id from a`, slug); err != nil {
		t.Fatal(err)
	}
}

// A link started before a suspension can't finish during it, and the
// answer says why; the state is left for after a lift.
func TestLinkRefusedWhileSuspended(t *testing.T) {
	h := newPaymentsHarness(t, payments.NewFake())
	owner := h.signupWithMFA("owner@example.com")
	h.createOrganizer(owner, "acme")
	state, code := h.startLink(owner, "acme")
	suspendBySQL(t, h, "acme")
	if res := h.completeLink(owner, state, code); res.StatusCode != 403 || h.errorCode(res) != "organizer_suspended" {
		t.Fatalf("complete while suspended: %d %q, want 403 organizer_suspended", res.StatusCode, h.errorCode(res))
	}
	if n := h.count(`select count(*) from stripe_link_states where state_hash = sha256($1::text::bytea)`, state); n != 1 {
		t.Fatalf("state rows %d, want the state kept", n)
	}
}
