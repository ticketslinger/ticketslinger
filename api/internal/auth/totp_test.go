package auth_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func TestTOTPEnrollThenLoginNeedsCode(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	_, u, _ := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	secret, _, err := s.StartTOTP(ctx, u.ID, "a long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	code, _ := totp.GenerateCode(secret, time.Now())
	if err := s.ConfirmTOTP(ctx, u.ID, code); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", "", ""); !errors.Is(err, auth.ErrTOTPRequired) {
		t.Fatalf("got %v, want ErrTOTPRequired", err)
	}
	next, _ := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	sess, _, err := s.Login(ctx, "a@example.com", "a long passphrase", next, "", "")
	if err != nil || !sess.MFA {
		t.Fatalf("login with code: mfa=%v err=%v", sess.MFA, err)
	}
}

func TestTOTPCodeCannotBeReused(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	_, u, _ := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	secret, _, _ := s.StartTOTP(ctx, u.ID, "a long passphrase")
	c1, _ := totp.GenerateCode(secret, time.Now())
	_ = s.ConfirmTOTP(ctx, u.ID, c1)
	c2, _ := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", c2, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", c2, "", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("replayed code: got %v, want ErrBadCredentials", err)
	}
}

func enrolled(t *testing.T) (*auth.Service, auth.User, string) {
	t.Helper()
	s, _ := newSvc(t)
	ctx := context.Background()
	_, u, _ := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	secret, _, _ := s.StartTOTP(ctx, u.ID, "a long passphrase")
	c, _ := totp.GenerateCode(secret, time.Now())
	if err := s.ConfirmTOTP(ctx, u.ID, c); err != nil {
		t.Fatal(err)
	}
	return s, u, secret
}

func TestTOTPConcurrentSameCodeOnlyOneWins(t *testing.T) {
	s, _, secret := enrolled(t)
	code, _ := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Login(context.Background(), "a@example.com", "a long passphrase", code, "", ""); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d concurrent logins succeeded with one code, want 1", ok)
	}
}

func TestTOTPWrongCodeIsBadCredentials(t *testing.T) {
	s, _, _ := enrolled(t)
	if _, _, err := s.Login(context.Background(), "a@example.com", "a long passphrase", "000000", "", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("got %v, want ErrBadCredentials", err)
	}
}

func TestTOTPStartWhileEnabledIsRefused(t *testing.T) {
	s, u, _ := enrolled(t)
	if _, _, err := s.StartTOTP(context.Background(), u.ID, "a long passphrase"); !errors.Is(err, auth.ErrTOTPAlreadyEnabled) {
		t.Fatalf("got %v, want ErrTOTPAlreadyEnabled", err)
	}
}

func TestTOTPConfirmRejectsWrongCode(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	_, u, _ := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	if err := s.ConfirmTOTP(ctx, u.ID, "123456"); !errors.Is(err, auth.ErrTOTPNotStarted) {
		t.Fatalf("before start: got %v, want ErrTOTPNotStarted", err)
	}
	_, _, _ = s.StartTOTP(ctx, u.ID, "a long passphrase")
	if err := s.ConfirmTOTP(ctx, u.ID, "000000"); !errors.Is(err, auth.ErrBadCode) {
		t.Fatalf("got %v, want ErrBadCode", err)
	}
}

func TestTOTPSecretEncryptedAtRest(t *testing.T) {
	pool := testdb.New(t)
	s := auth.NewService(pool, &mailer.Memory{}, auth.Options{SecretKey: testKey})
	ctx := context.Background()
	_, u, _ := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	secret, _, err := s.StartTOTP(ctx, u.ID, "a long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT totp_secret FROM users WHERE id = $1`, u.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) < 12+16 || bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("secret stored in the clear: %q", raw)
	}
}

func TestTOTPStartNeedsPassword(t *testing.T) {
	pool := testdb.New(t)
	s := auth.NewService(pool, &mailer.Memory{}, auth.Options{SecretKey: testKey})
	ctx := context.Background()
	_, u, _ := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	if _, _, err := s.StartTOTP(ctx, u.ID, "wrong"); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("got %v, want ErrBadCredentials", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT failed_logins FROM users WHERE id = $1`, u.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("failed_logins = %d, want 1", n)
	}
	if secret, _, err := s.StartTOTP(ctx, u.ID, "a long passphrase"); err != nil || secret == "" {
		t.Fatalf("right password: %q %v", secret, err)
	}
}

func TestTOTPSecretBoundToUser(t *testing.T) {
	pool := testdb.New(t)
	s := auth.NewService(pool, &mailer.Memory{}, auth.Options{SecretKey: testKey})
	ctx := context.Background()
	_, a, _ := s.Signup(ctx, "a@example.com", "a long passphrase", "A", "", "")
	_, b, _ := s.Signup(ctx, "b@example.com", "a long passphrase", "B", "", "")
	secret, _, _ := s.StartTOTP(ctx, a.ID, "a long passphrase")
	// Copy A's sealed secret onto B's row, as an attacker with DB write access might.
	if _, err := pool.Exec(ctx, `UPDATE users SET totp_secret = (SELECT totp_secret FROM users WHERE id = $1) WHERE id = $2`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	code, _ := totp.GenerateCode(secret, time.Now())
	if err := s.ConfirmTOTP(ctx, b.ID, code); err == nil || errors.Is(err, auth.ErrBadCode) {
		t.Fatalf("copied secret opened for another user: %v", err)
	}
}
