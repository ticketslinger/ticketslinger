package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func newSvc(t *testing.T) (*auth.Service, *mailer.Memory) {
	m := &mailer.Memory{}
	return auth.NewService(testdb.New(t), m, auth.Options{RPID: "localhost", Origin: "http://localhost:5173"}), m
}

func TestSignupThenLogin(t *testing.T) {
	s, m := newSvc(t)
	ctx := context.Background()
	if _, _, err := s.Signup(ctx, "Ann@Example.com", "a long passphrase", "Ann", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(m.Sent) != 1 {
		t.Fatalf("want 1 verification email, got %d", len(m.Sent))
	}
	sess, u, err := s.Login(ctx, "ann@example.com", "a long passphrase", "", "", "")
	if err != nil || sess.Token == "" || u.Email != "ann@example.com" || sess.MFA {
		t.Fatalf("login: %+v %+v %v", sess, u, err)
	}
	got, _, err := s.Authenticate(ctx, sess.Token)
	if err != nil || got.ID != u.ID {
		t.Fatalf("authenticate: %v", err)
	}
}

func TestSignupRejectsDuplicateEmail(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	_, _, _ = s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	if _, _, err := s.Signup(ctx, "A@EXAMPLE.COM", "another long one", "A", "", ""); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("got %v, want ErrEmailTaken", err)
	}
}

func TestLoginLockoutAfterFiveFailures(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	_, _, _ = s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	for i := 0; i < 5; i++ {
		_, _, _ = s.Login(ctx, "a@example.com", "wrong", "", "", "")
	}
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", "", ""); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("got %v, want ErrLocked after 5 failures", err)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	sess, _, _ := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	_ = s.Logout(ctx, sess.Token)
	if _, _, err := s.Authenticate(ctx, sess.Token); !errors.Is(err, auth.ErrBadToken) {
		t.Fatalf("got %v, want ErrBadToken", err)
	}
}

func TestVerifyEmailTokenIsSingleUse(t *testing.T) {
	s, m := newSvc(t)
	ctx := context.Background()
	_, _, _ = s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	tok := m.LastToken()
	if err := s.VerifyEmail(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyEmail(ctx, tok); !errors.Is(err, auth.ErrBadToken) {
		t.Fatalf("reuse: got %v, want ErrBadToken", err)
	}
}

func TestSessionSlidesOnUse(t *testing.T) {
	pool := testdb.New(t)
	s := auth.NewService(pool, &mailer.Memory{}, auth.Options{Origin: "http://localhost:5173"})
	ctx := context.Background()
	sess, _, err := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET expires_at = now() + interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	_, got, err := s.Authenticate(ctx, sess.Token)
	if err != nil || time.Until(got.ExpiresAt) < 29*24*time.Hour {
		t.Fatalf("session did not slide: %v %v", got.ExpiresAt, err)
	}
}
