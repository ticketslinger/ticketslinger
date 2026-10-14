package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func login(s *auth.Service, password, token, ip string) error {
	_, _, err := s.Login(context.Background(), "a@example.com", password, "", "", token, ip, "")
	return err
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

// One host usually owns a whole IPv6 /64, so the /64 is the address.
func TestLockoutKeysIPv6ByPrefix(t *testing.T) {
	s := newGuardedSvc(t, nil)
	for i := 0; i < 5; i++ {
		_ = login(s, "wrong", "", "2001:db8:1:2::"+string(rune('a'+i)))
	}
	if err := login(s, pw, "", "2001:db8:1:2:ffff::1"); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("same /64: %v, want ErrLocked", err)
	}
	if err := login(s, pw, "", "2001:db8:1:3::1"); err != nil {
		t.Fatalf("another /64: %v, want signed in", err)
	}
}

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
		t.Fatalf("4th with a bad token: %v, want turnstile.ErrFailed", err)
	}
	if err := login(s, "wrong", "ok", "203.0.113.1"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("4th with a token, wrong password: %v", err)
	}
	if err := login(s, "wrong", "ok", "203.0.113.1"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("5th with a token, wrong password: %v", err)
	}
	if err := login(s, pw, "ok", "203.0.113.1"); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("token attempts count toward the lock: %v", err)
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
		ip := "203.0.113." + string(rune('1'+i/2)) // two failures from each of five addresses
		if err := login(s, "wrong", "", ip); !errors.Is(err, auth.ErrBadCredentials) {
			t.Fatalf("failure %d from %s: %v", i, ip, err)
		}
	}
	auth.ResetLimits(s) // the per-email limiter allows 10 a minute
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
		_ = login(s, "wrong", "", "203.0.113."+string(rune('1'+i/2)))
	}
	auth.ResetLimits(s)
	if err := login(s, pw, "", "198.51.100.7"); err != nil {
		t.Fatalf("got %v, want signed in", err)
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
	s := auth.NewService(pool, &mailer.Memory{}, auth.Options{Origin: "http://localhost:5173"})
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
