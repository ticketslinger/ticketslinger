package auth_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp/totp"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
)

func signupWithTOTP(t *testing.T, s *auth.Service, email string) auth.User {
	t.Helper()
	ctx := context.Background()
	_, u, err := s.Signup(ctx, email, "a long passphrase", "A", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.StartTOTP(ctx, u.ID, false, "a long passphrase", "")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := totp.GenerateCode(secret, time.Now())
	if err := s.ConfirmTOTP(ctx, u.ID, false, c); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRecoveryCodeIsSingleUse(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	u := signupWithTOTP(t, s, "a@example.com")
	codes, err := s.GenerateRecoveryCodes(ctx, u.ID, true)
	if err != nil || len(codes) != 10 {
		t.Fatalf("codes=%d err=%v", len(codes), err)
	}
	shape := regexp.MustCompile(`^[2-9A-HJKMNP-TV-Z]{4}-[2-9A-HJKMNP-TV-Z]{4}-[2-9A-HJKMNP-TV-Z]{4}$`)
	for _, c := range codes {
		if !shape.MatchString(c) {
			t.Fatalf("bad code shape %q", c)
		}
	}
	// Typed sloppily: lower case, spaces instead of hyphens.
	sloppy := strings.ToLower(strings.ReplaceAll(codes[0], "-", " "))
	sess, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", sloppy, "", "", "")
	if err != nil || !sess.MFA {
		t.Fatalf("first use: mfa=%v err=%v", sess.MFA, err)
	}
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", codes[0], "", "", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("reuse: got %v, want ErrBadCredentials", err)
	}
	// The other nine still work.
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", codes[1], "", "", ""); err != nil {
		t.Fatalf("second code: %v", err)
	}
}

func TestWrongRecoveryCodesLockTheAccount(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	u := signupWithTOTP(t, s, "a@example.com")
	_, _ = s.GenerateRecoveryCodes(ctx, u.ID, true)
	for i := 0; i < 5; i++ {
		if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", "aaaa-bbbb-cccc", "", "", ""); !errors.Is(err, auth.ErrBadCredentials) {
			t.Fatalf("attempt %d: got %v, want ErrBadCredentials", i+1, err)
		}
	}
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", "", "", "", ""); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("got %v, want ErrLocked", err)
	}
}

func TestRecoveryCodesNeedMFAAndAFactor(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	_, u, _ := s.Signup(ctx, "b@example.com", "a long passphrase", "B", "", "", "")
	if _, err := s.GenerateRecoveryCodes(ctx, u.ID, true); !errors.Is(err, auth.ErrNoSecondFactor) {
		t.Fatalf("no factor: got %v", err)
	}
	v := signupWithTOTP(t, s, "c@example.com")
	if _, err := s.GenerateRecoveryCodes(ctx, v.ID, false); !errors.Is(err, auth.ErrMFARequired) {
		t.Fatalf("no mfa: got %v", err)
	}
}

func TestRegeneratingReplacesOldCodes(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	u := signupWithTOTP(t, s, "a@example.com")
	old, _ := s.GenerateRecoveryCodes(ctx, u.ID, true)
	fresh, _ := s.GenerateRecoveryCodes(ctx, u.ID, true)
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", old[0], "", "", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("old code after regenerate: got %v", err)
	}
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", fresh[0], "", "", ""); err != nil {
		t.Fatalf("new code: %v", err)
	}
}

func TestRecoveryCodeForPasskeyOnlyAccount(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "p@example.com")
	register(t, s, u, newDevice(u.ID))
	codes, err := s.GenerateRecoveryCodes(ctx, u.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	// Without a code, behaviour is unchanged: a plain, non-MFA session.
	if sess, _, err := s.Login(ctx, "p@example.com", "a long passphrase", "", "", "", "", ""); err != nil || sess.MFA {
		t.Fatalf("no code: mfa=%v err=%v", sess.MFA, err)
	}
	sess, _, err := s.Login(ctx, "p@example.com", "a long passphrase", "", codes[0], "", "", "")
	if err != nil || !sess.MFA {
		t.Fatalf("with code: mfa=%v err=%v", sess.MFA, err)
	}
}

func TestRecoveryAndTOTPTogetherAreRefused(t *testing.T) {
	s, _ := newSvc(t)
	if _, _, err := s.Login(context.Background(), "a@example.com", "x", "123456", "aaaa-bbbb-cccc", "", "", ""); !errors.Is(err, auth.ErrOneSecondStep) {
		t.Fatalf("got %v, want ErrOneSecondStep", err)
	}
}

func TestRecoveryCodeConcurrentOnlyOneWins(t *testing.T) {
	s, _ := newSvc(t)
	u := signupWithTOTP(t, s, "a@example.com")
	codes, _ := s.GenerateRecoveryCodes(context.Background(), u.ID, true)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 4; i++ { // below the 5-failure lockout, so losers fail on the code, not on ErrLocked
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Login(context.Background(), "a@example.com", "a long passphrase", "", codes[0], "", "", ""); err == nil {
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

func TestStaleRecoveryCodesFailAndCountTowardLockout(t *testing.T) {
	s, pool := newSvcPool(t)
	ctx := context.Background()
	u := signupWithTOTP(t, s, "a@example.com")
	codes, _ := s.GenerateRecoveryCodes(ctx, u.ID, true)
	// The only factor goes away; its codes must not outlive it.
	if _, err := pool.Exec(ctx, `UPDATE users SET totp_enabled_at = NULL, totp_secret = NULL WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", codes[i], "", "", ""); !errors.Is(err, auth.ErrBadCredentials) {
			t.Fatalf("stale code %d: got %v, want ErrBadCredentials", i, err)
		}
	}
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", "", "", "", ""); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("got %v, want ErrLocked", err)
	}
}

func TestRecoveryGenerationIsRateLimited(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	u := signupWithTOTP(t, s, "a@example.com")
	for i := 0; i < 5; i++ {
		if _, err := s.GenerateRecoveryCodes(ctx, u.ID, true); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if _, err := s.GenerateRecoveryCodes(ctx, u.ID, true); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("sixth call: got %v, want ErrRateLimited", err)
	}
}

// Hashing is slow; it must not happen while the user row is locked.
func TestRecoveryHashingHappensOutsideTheRowLock(t *testing.T) {
	s, pool := newSvcPool(t)
	ctx := context.Background()
	u := signupWithTOTP(t, s, "a@example.com")
	called := 0
	auth.SetBeforeHash(s, func() {
		called++
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE NOWAIT`, u.ID)
			return err
		})
		if err != nil {
			t.Errorf("user row locked before hashing: %v", err)
		}
	})
	if _, err := s.GenerateRecoveryCodes(ctx, u.ID, true); err != nil || called != 10 {
		t.Fatalf("err=%v hashes seen=%d", err, called)
	}
}
