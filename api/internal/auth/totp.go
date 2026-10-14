package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

var (
	ErrTOTPAlreadyEnabled = errors.New("auth: totp already enabled")
	ErrTOTPNotStarted     = errors.New("auth: totp not started")
	ErrBadCode            = errors.New("auth: bad code")
)

const totpPeriod = 30

func totpOpts() totp.ValidateOpts {
	return totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
}

// seal encrypts with AES-256-GCM; the random 12-byte nonce is prepended.
// aad (the owner's user id) binds the ciphertext to its row.
func seal(key, aad, plain []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, aad), nil
}

func open(key, aad, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("auth: sealed value too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("auth: secret key must be 32 bytes")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// matchStep returns the time step of the code within ±1 step of now, or false.
// All three candidates are always compared so timing doesn't reveal which matched.
func matchStep(secret, code string, now time.Time) (int64, bool) {
	var (
		step  int64
		found bool
	)
	for d := -1; d <= 1; d++ {
		t := now.Add(time.Duration(d*totpPeriod) * time.Second)
		want, err := totp.GenerateCodeCustom(secret, t, totpOpts())
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			step, found = t.Unix()/totpPeriod, true
		}
	}
	return step, found
}

// checkTOTP verifies code against the user's stored secret and returns the
// matching step. It does not record it; the caller does that atomically with
// recordTOTPStep. A step at or before lastStep is a replay.
func (s *Service) checkTOTP(userID uuid.UUID, sealed []byte, lastStep *int64, code string) (int64, bool, error) {
	secret, err := open(s.opts.SecretKey, userID[:], sealed)
	if err != nil {
		return 0, false, err
	}
	step, ok := matchStep(string(secret), code, time.Now())
	if !ok || (lastStep != nil && step <= *lastStep) {
		return 0, false, nil
	}
	return step, true, nil
}

// recordTOTPStep stores an accepted step. Zero rows means another request
// already used this step or a later one, so the code is a replay.
func recordTOTPStep(ctx context.Context, tx pgx.Tx, id uuid.UUID, step int64, enable bool) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE users SET totp_last_step = $2,
		        totp_enabled_at = CASE WHEN $3 THEN now() ELSE totp_enabled_at END
		  WHERE id = $1 AND (totp_last_step IS NULL OR totp_last_step < $2)`, id, step, enable)
	return tag.RowsAffected() == 1, err
}

// StartTOTP begins enrolment: it stores a fresh encrypted secret, not yet
// enabled, and returns it with the otpauth:// URL for a QR code. Calling it
// again before confirming replaces the pending secret. A password session
// must send the current password, so a stolen session token can't lock the
// owner out (a wrong one counts toward the login lockout), and may only do
// so while the account has no second factor (see checkEnrolment). An mfa
// session needs no password.
func (s *Service) StartTOTP(ctx context.Context, userID uuid.UUID, mfa bool, password string) (string, string, error) {
	if err := s.checkEnrolment(ctx, userID, mfa); err != nil {
		return "", "", err
	}
	var email string
	var err error
	switch {
	case mfa:
		err = s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email)
	case password == "":
		return "", "", ErrPasswordRequired
	default:
		email, err = s.reauthenticate(ctx, userID, password)
	}
	if err != nil {
		return "", "", err
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "TicketSlinger", AccountName: email})
	if err != nil {
		return "", "", err
	}
	sealed, err := seal(s.opts.SecretKey, userID[:], []byte(key.Secret()))
	if err != nil {
		return "", "", err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_secret = $2, totp_last_step = NULL WHERE id = $1 AND totp_enabled_at IS NULL`,
		userID, sealed)
	if err != nil {
		return "", "", err
	}
	if tag.RowsAffected() == 0 {
		return "", "", ErrTOTPAlreadyEnabled
	}
	return key.Secret(), key.URL(), nil
}

// ConfirmTOTP proves the user's authenticator works and switches TOTP on.
func (s *Service) ConfirmTOTP(ctx context.Context, userID uuid.UUID, code string) error {
	if !s.emLimit.allow("totp:"+userID.String(), time.Now()) {
		return ErrRateLimited
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var sealed []byte
		var enabled bool
		if err := tx.QueryRow(ctx,
			`SELECT totp_secret, totp_enabled_at IS NOT NULL FROM users WHERE id = $1 FOR UPDATE`, userID).
			Scan(&sealed, &enabled); err != nil {
			return err
		}
		if enabled {
			return ErrTOTPAlreadyEnabled
		}
		if sealed == nil {
			return ErrTOTPNotStarted
		}
		step, ok, err := s.checkTOTP(userID, sealed, nil, code)
		if err != nil {
			return err
		}
		if !ok {
			return ErrBadCode
		}
		if ok, err := recordTOTPStep(ctx, tx, userID, step, true); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("auth: recording totp step: no row")
			}
			return err
		}
		return nil
	})
}
