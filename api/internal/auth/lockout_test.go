package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
	"github.com/ticketslinger/ticketslinger/api/internal/turnstile"
)

const pw = "a long passphrase"

// fakeTurnstile accepts the token "ok" and nothing else.
func fakeTurnstile(t *testing.T) *turnstile.Verifier {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("response") == "ok" {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	}))
	t.Cleanup(srv.Close)
	return &turnstile.Verifier{Secret: "s", URL: srv.URL, Client: srv.Client()}
}

func newGuardedSvc(t *testing.T, v *turnstile.Verifier) *auth.Service {
	s := auth.NewService(testdb.New(t), &mailer.Memory{}, auth.Options{Origin: "http://localhost:5173", SecretKey: testKey, Turnstile: v})
	if _, _, err := s.Signup(context.Background(), "a@example.com", pw, "A", "ok", "", ""); err != nil {
		t.Fatal(err)
	}
	return s
}

func loginAs(s *auth.Service, email, password, token, ip string) error {
	_, _, err := s.Login(context.Background(), email, password, "", "", token, ip, "")
	return err
}

func login(s *auth.Service, password, token, ip string) error {
	return loginAs(s, "a@example.com", password, token, ip)
}

func TestLockoutIsPerAddress(t *testing.T) {
	s := newGuardedSvc(t, nil)
	for i := 0; i < 5; i++ {
		if err := login(s, "wrong", "", "203.0.113.1"); !errors.Is(err, auth.ErrBadCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if err := login(s, pw, "", "203.0.113.1"); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("same address: %v, want ErrLocked", err)
	}
	if err := login(s, pw, "", "198.51.100.7"); err != nil {
		t.Fatalf("another address: %v, want signed in", err)
	}
}

// A stranger hammering from one address can't rate-limit the owner out
// everywhere: the per-email limit is per email and address.
func TestHammeringOneAddressLeavesOthersAlone(t *testing.T) {
	s := newGuardedSvc(t, nil)
	for i := 0; i < 12; i++ {
		_ = login(s, "wrong", "", "203.0.113.1")
	}
	if err := login(s, pw, "", "198.51.100.7"); err != nil {
		t.Fatalf("owner from another address: %v, want signed in", err)
	}
}

// One host usually owns a whole IPv6 /64, so the /64 is the address.
func TestLockoutKeysIPv6ByPrefix(t *testing.T) {
	s := newGuardedSvc(t, nil)
	for i := 0; i < 5; i++ {
		_ = login(s, "wrong", "", "2001:db8:1:2::"+strconv.Itoa(i+1))
	}
	if err := login(s, pw, "", "2001:db8:1:2:ffff::1"); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("same /64: %v, want ErrLocked", err)
	}
	if err := login(s, pw, "", "2001:db8:1:3::1"); err != nil {
		t.Fatalf("another /64: %v, want signed in", err)
	}
}

// Every attempt counts, including one refused for a missing or bad token.
func TestTurnstileAfterThreeFailures(t *testing.T) {
	s := newGuardedSvc(t, fakeTurnstile(t))
	for i := 0; i < 3; i++ {
		if err := login(s, "wrong", "", "203.0.113.1"); !errors.Is(err, auth.ErrBadCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if err := login(s, pw, "", "203.0.113.1"); !errors.Is(err, auth.ErrTurnstileRequired) {
		t.Fatalf("4th without a token: %v, want ErrTurnstileRequired", err)
	}
	if err := login(s, pw, "forged", "203.0.113.1"); !errors.Is(err, turnstile.ErrFailed) {
		t.Fatalf("5th with a bad token: %v, want turnstile.ErrFailed", err)
	}
	if err := login(s, pw, "ok", "203.0.113.1"); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("refused tokens count toward the lock: %v", err)
	}
	if err := login(s, pw, "", "198.51.100.7"); err != nil {
		t.Fatalf("another address needs no token: %v", err)
	}
}

// Ten failures across addresses put the whole account behind Turnstile,
// from any address, but never lock it: the owner still gets in.
func TestAccountWideBrake(t *testing.T) {
	s := newGuardedSvc(t, fakeTurnstile(t))
	for i := 0; i < 10; i++ {
		ip := "203.0.113." + strconv.Itoa(1+i/2) // two failures from each of five addresses
		if err := login(s, "wrong", "", ip); !errors.Is(err, auth.ErrBadCredentials) {
			t.Fatalf("failure %d from %s: %v", i, ip, err)
		}
	}
	if err := login(s, pw, "", "198.51.100.7"); !errors.Is(err, auth.ErrTurnstileRequired) {
		t.Fatalf("fresh address after 10 failures: %v, want ErrTurnstileRequired", err)
	}
	if err := login(s, pw, "ok", "198.51.100.7"); err != nil {
		t.Fatalf("owner with a token: %v, want signed in", err)
	}
}

func TestAccountWideBrakeIsOffWithoutTurnstile(t *testing.T) {
	s := newGuardedSvc(t, nil)
	for i := 0; i < 10; i++ {
		_ = login(s, "wrong", "", "203.0.113."+strconv.Itoa(1+i/2))
	}
	if err := login(s, pw, "", "198.51.100.7"); err != nil {
		t.Fatalf("got %v, want signed in", err)
	}
}

// An email with no account goes through the same counting, the same check
// and the same lock as a real one, so the answers don't say which is which.
func TestUnknownEmailLooksLikeARealOne(t *testing.T) {
	s := newGuardedSvc(t, fakeTurnstile(t))
	steps := []struct{ password, token string }{
		{"wrong", ""}, {"wrong", ""}, {"wrong", ""}, {"wrong", ""}, {"wrong", "forged"}, {"wrong", "ok"},
	}
	run := func(email string) []error {
		var got []error
		for _, st := range steps {
			got = append(got, loginAs(s, email, st.password, st.token, "203.0.113.1"))
		}
		for i := 0; i < 10; i++ { // and the account-wide brake, from fresh addresses
			_ = loginAs(s, email, "wrong", "", "198.51.100."+strconv.Itoa(1+i/2))
		}
		return append(got, loginAs(s, email, "wrong", "", "192.0.2.1"))
	}
	real, ghost := run("a@example.com"), run("nobody@example.com")
	want := []error{auth.ErrBadCredentials, auth.ErrBadCredentials, auth.ErrBadCredentials,
		auth.ErrTurnstileRequired, turnstile.ErrFailed, auth.ErrLocked, auth.ErrTurnstileRequired}
	for i := range want {
		if !errors.Is(real[i], want[i]) || !errors.Is(ghost[i], want[i]) {
			t.Errorf("step %d: real %v, unknown %v, want %v", i, real[i], ghost[i], want[i])
		}
	}
}

// Parallel guesses can't race past the limit: each attempt is counted
// before its password is checked.
func TestParallelGuessesStopAtTheLimit(t *testing.T) {
	s := newGuardedSvc(t, nil)
	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = login(s, "wrong", "", "203.0.113.1") })
	}
	wg.Wait()
	checked := 0
	for _, err := range errs {
		switch {
		case errors.Is(err, auth.ErrBadCredentials):
			checked++
		case !errors.Is(err, auth.ErrLocked):
			t.Fatalf("got %v", err)
		}
	}
	if checked > 5 {
		t.Fatalf("%d of %d parallel guesses reached the password check, want at most 5", checked, n)
	}
}

func TestSignupNeedsTurnstileWhenConfigured(t *testing.T) {
	s := auth.NewService(testdb.New(t), &mailer.Memory{}, auth.Options{Origin: "http://localhost:5173", Turnstile: fakeTurnstile(t)})
	ctx := context.Background()
	for _, tok := range []string{"", "forged"} {
		if _, _, err := s.Signup(ctx, "a@example.com", pw, "A", tok, "", ""); !errors.Is(err, turnstile.ErrFailed) {
			t.Fatalf("token %q: %v, want turnstile.ErrFailed", tok, err)
		}
	}
	if _, _, err := s.Signup(ctx, "a@example.com", pw, "A", "ok", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessClearsTheAddressCounter(t *testing.T) {
	s := newGuardedSvc(t, fakeTurnstile(t))
	for i := 0; i < 3; i++ {
		_ = login(s, "wrong", "", "203.0.113.1")
	}
	if err := login(s, pw, "ok", "203.0.113.1"); err != nil {
		t.Fatal(err)
	}
	if err := login(s, "wrong", "", "203.0.113.1"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("after a success: %v, want a plain failure, no token needed", err)
	}
}

// The password check before adding a second factor shares the address's counter.
func TestReauthenticationCountsPerAddress(t *testing.T) {
	s := newGuardedSvc(t, nil)
	_, u, err := s.Login(context.Background(), "a@example.com", pw, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := s.StartTOTP(context.Background(), u.ID, false, "wrong", "203.0.113.1"); !errors.Is(err, auth.ErrBadCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if err := login(s, pw, "", "203.0.113.1"); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("sign-in from that address: %v, want ErrLocked", err)
	}
	if _, _, err := s.StartTOTP(context.Background(), u.ID, false, pw, "198.51.100.7"); err != nil {
		t.Fatalf("another address: %v", err)
	}
}

// A count lapses 15 minutes after its last failure.
func TestFailuresLapseAfterTheWindow(t *testing.T) {
	pool := testdb.New(t)
	s := auth.NewService(pool, &mailer.Memory{}, auth.Options{Origin: "http://localhost:5173", SecretKey: testKey})
	if _, _, err := s.Signup(context.Background(), "a@example.com", pw, "A", "", "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		_ = login(s, "wrong", "", "203.0.113.1")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE login_failures SET updated_at = now() - interval '16 minutes'`); err != nil {
		t.Fatal(err)
	}
	_ = login(s, "wrong", "", "203.0.113.1")
	_ = login(s, "wrong", "", "203.0.113.1")
	if err := login(s, pw, "", "203.0.113.1"); err != nil {
		t.Fatalf("4 stale failures and 2 fresh: %v, want signed in", err)
	}
}

// Rows name the account by a keyed hash of its email, never the email or a
// plain hash of it.
func TestFailuresDontStoreTheEmail(t *testing.T) {
	pool := testdb.New(t)
	s := auth.NewService(pool, &mailer.Memory{}, auth.Options{SecretKey: testKey})
	_ = loginAs(s, "nobody@example.com", "wrong", "", "203.0.113.1")
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM login_failures WHERE length(account) = 32
		   AND account <> sha256('nobody@example.com') AND position('nobody' in encode(account, 'escape')) = 0`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows keyed by an HMAC: %d, %v", n, err)
	}
}
