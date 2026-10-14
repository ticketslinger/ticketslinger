// Package auth holds identity services: passwords, sessions and email verification.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

var (
	ErrEmailTaken       = errors.New("auth: email taken")
	ErrBadCredentials   = errors.New("auth: bad credentials")
	ErrTOTPRequired     = errors.New("auth: totp required")
	ErrLocked           = errors.New("auth: locked")
	ErrBadToken         = errors.New("auth: bad token")
	ErrRateLimited      = errors.New("auth: rate limited")
	ErrPasswordTooShort = errors.New("auth: password too short")
	ErrPasswordTooLong  = errors.New("auth: password too long")
	ErrInvalidName      = errors.New("auth: invalid name")
	ErrPasswordRequired = errors.New("auth: password required")
	ErrMFARequired      = errors.New("auth: mfa required")
)

const (
	minPassword     = 12
	maxPassword     = 256
	maxName         = 120
	sessionLifetime = 30 * 24 * time.Hour
	verifyLifetime  = 48 * time.Hour
	maxSessionAge   = 90 * 24 * time.Hour // absolute cap, however often a session slides
	maxFailures     = 5
	lockout         = 15 * time.Minute
)

type Session struct {
	ID        uuid.UUID
	Token     string // only set when the session is created
	MFA       bool
	ExpiresAt time.Time
}

type User struct {
	ID                         uuid.UUID
	Email, Name                string
	EmailVerified, TOTPEnabled bool
	HasPasskey                 bool
}

type Options struct {
	RPID   string // WebAuthn relying party ID
	Origin string // dashboard origin, used for links in email
	// SecretKey is the 32-byte AES-256 key that encrypts TOTP secrets at rest.
	SecretKey []byte
}

type Service struct {
	pool    *pgxpool.Pool
	mail    mailer.Mailer
	opts    Options
	ipLimit *limiter
	emLimit *limiter
	// recLimit caps recovery-code generation per user; each call costs ten Argon2 hashes.
	recLimit   *limiter
	beforeHash func()             // test hook, nil in production
	wa         *webauthn.WebAuthn // nil when RPID or Origin is unusable; passkey calls then return waErr
	waErr      error
}

func NewService(pool *pgxpool.Pool, m mailer.Mailer, o Options) *Service {
	s := &Service{pool: pool, mail: m, opts: o,
		ipLimit: newLimiter(20, time.Minute), emLimit: newLimiter(10, time.Minute),
		recLimit: newLimiter(5, time.Hour)}
	s.wa, s.waErr = newWebAuthn(o)
	return s
}

// dummyHash is checked when no user matches, so a login takes as long
// whether or not the email exists.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("not a real password, only for timing")
	if err != nil {
		panic(err)
	}
	return h
})

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// newToken returns a random base64url token and its SHA-256.
func newToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	return tok, hashToken(tok), nil
}

func hashToken(tok string) []byte { h := sha256.Sum256([]byte(tok)); return h[:] }

func (s *Service) Signup(ctx context.Context, email, password, name, ip, ua string) (Session, User, error) {
	if !s.ipLimit.allow("signup:"+ip, time.Now()) {
		return Session{}, User{}, ErrRateLimited
	}
	email, name = normEmail(email), strings.TrimSpace(name)
	switch n := utf8.RuneCountInString(password); {
	case n < minPassword:
		return Session{}, User{}, ErrPasswordTooShort
	case n > maxPassword:
		return Session{}, User{}, ErrPasswordTooLong
	}
	if name == "" || utf8.RuneCountInString(name) > maxName {
		return Session{}, User{}, ErrInvalidName
	}
	hash, err := hashPassword(ctx, password)
	if err != nil {
		return Session{}, User{}, err
	}
	verifyTok, verifyHash, err := newToken()
	if err != nil {
		return Session{}, User{}, err
	}
	u := User{Email: email, Name: name}
	var sess Session
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id, email`,
			email, name, hash).Scan(&u.ID, &u.Email)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrEmailTaken
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO email_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1, $2, 'verify', $3)`,
			verifyHash, u.ID, time.Now().Add(verifyLifetime)); err != nil {
			return err
		}
		sess, err = createSession(ctx, tx, u.ID, false, ip, ua)
		return err
	})
	if err != nil {
		return Session{}, User{}, err
	}
	link := s.opts.Origin + "/verify-email?token=" + url.QueryEscape(verifyTok)
	// The name is user-supplied, so it stays out of the email.
	body := "Hi,\n\nConfirm your email for TicketSlinger by opening this link:\n\n" + link +
		"\n\nIt works for 48 hours. If you didn't sign up, you can ignore this email.\n"
	// The account exists either way; a failed send must not fail the signup.
	if err := s.mail.Send(ctx, email, "Confirm your email", body); err != nil {
		slog.ErrorContext(ctx, "verification email not sent", "user", u.ID, "err", err)
	}
	return sess, u, nil
}

func (s *Service) Login(ctx context.Context, email, password, totpCode, recoveryCode, ip, ua string) (Session, User, error) {
	if totpCode != "" && recoveryCode != "" {
		return Session{}, User{}, ErrOneSecondStep
	}
	email = normEmail(email)
	now := time.Now()
	if !s.ipLimit.allow("login:"+ip, now) || !s.emLimit.allow(email, now) {
		return Session{}, User{}, ErrRateLimited
	}
	var (
		u      User
		hash   *string
		totpOn bool
		sealed []byte
		last   *int64
		locked bool
		hasKey bool
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, name, email_verified_at IS NOT NULL, totp_enabled_at IS NOT NULL,
		        password_hash, coalesce(locked_until > now(), false), totp_secret, totp_last_step,
		        EXISTS (SELECT 1 FROM passkeys p WHERE p.user_id = users.id)
		   FROM users WHERE email = $1`, email).
		Scan(&u.ID, &u.Email, &u.Name, &u.EmailVerified, &totpOn, &hash, &locked, &sealed, &last, &hasKey)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hash == nil) {
		_, _ = checkPassword(ctx, dummyHash(), password)
		return Session{}, User{}, ErrBadCredentials
	}
	if err != nil {
		return Session{}, User{}, err
	}
	if locked {
		return Session{}, User{}, ErrLocked
	}
	u.TOTPEnabled = totpOn
	ok, err := checkPassword(ctx, *hash, password)
	if err != nil {
		return Session{}, User{}, err
	}
	if !ok {
		return Session{}, User{}, s.recordFailure(ctx, u.ID)
	}
	var codeID uuid.UUID
	if recoveryCode != "" {
		if !totpOn && !hasKey { // stale codes from a factor since removed
			return Session{}, User{}, s.recordFailure(ctx, u.ID)
		}
		// Accepted for any account with a second factor; it stands in for the TOTP code or the passkey.
		if codeID, err = s.matchRecovery(ctx, u.ID, recoveryCode); err != nil {
			return Session{}, User{}, err
		}
		if codeID == uuid.Nil {
			return Session{}, User{}, s.recordFailure(ctx, u.ID)
		}
	} else if totpOn && totpCode == "" {
		return Session{}, User{}, ErrTOTPRequired
	}
	var step int64
	if totpOn && recoveryCode == "" {
		var ok bool
		if step, ok, err = s.checkTOTP(u.ID, sealed, last, totpCode); err != nil {
			return Session{}, User{}, err
		}
		if !ok {
			return Session{}, User{}, s.recordFailure(ctx, u.ID)
		}
	}
	var sess Session
	replay := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if codeID != uuid.Nil {
			// Conditional update: of two logins racing with one recovery code, only one gets a row.
			tag, err := tx.Exec(ctx, `UPDATE recovery_codes SET used_at = now() WHERE id = $1 AND used_at IS NULL`, codeID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				replay = true
				return nil
			}
		} else if totpOn {
			// Conditional update: of two logins racing with one code, only one gets a row.
			ok, err := recordTOTPStep(ctx, tx, u.ID, step, false)
			if err != nil {
				return err
			}
			if !ok {
				replay = true
				return nil
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET failed_logins = 0 WHERE id = $1`, u.ID); err != nil {
			return err
		}
		sess, err = createSession(ctx, tx, u.ID, totpOn || codeID != uuid.Nil, ip, ua)
		return err
	})
	if err == nil && replay {
		return Session{}, User{}, s.recordFailure(ctx, u.ID)
	}
	return sess, u, err
}

// recordFailure counts a failed login and locks the account at the limit.
func (s *Service) recordFailure(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET
		   locked_until  = CASE WHEN failed_logins + 1 >= $2 THEN now() + $3::interval ELSE locked_until END,
		   failed_logins = CASE WHEN failed_logins + 1 >= $2 THEN 0 ELSE failed_logins + 1 END
		 WHERE id = $1`, id, maxFailures, lockout.String()); err != nil {
		return err
	}
	return ErrBadCredentials
}

func createSession(ctx context.Context, tx pgx.Tx, userID uuid.UUID, mfa bool, ip, ua string) (Session, error) {
	tok, h, err := newToken()
	if err != nil {
		return Session{}, err
	}
	sess := Session{Token: tok, MFA: mfa}
	err = tx.QueryRow(ctx,
		`INSERT INTO sessions (user_id, token_hash, mfa, ip, user_agent, expires_at)
		 VALUES ($1, $2, $3, NULLIF($4, '')::inet, NULLIF($5, ''), now() + $6::interval)
		 RETURNING id, expires_at`,
		userID, h, mfa, ip, ua, sessionLifetime.String()).Scan(&sess.ID, &sess.ExpiresAt)
	return sess, err
}

// Authenticate resolves a session token. Sessions slide: each day of use
// pushes expiry back out to 30 days, but none lives past 90 days from creation.
func (s *Service) Authenticate(ctx context.Context, token string) (User, Session, error) {
	var u User
	var sess Session
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.mfa, s.expires_at,
		        u.id, u.email, u.name, u.email_verified_at IS NOT NULL, u.totp_enabled_at IS NOT NULL,
		        EXISTS (SELECT 1 FROM passkeys p WHERE p.user_id = u.id)
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = $1 AND s.expires_at > now() AND s.created_at > now() - $2::interval`,
		hashToken(token), maxSessionAge.String()).
		Scan(&sess.ID, &sess.MFA, &sess.ExpiresAt, &u.ID, &u.Email, &u.Name, &u.EmailVerified, &u.TOTPEnabled, &u.HasPasskey)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, Session{}, ErrBadToken
	}
	if err != nil {
		return User{}, Session{}, err
	}
	if time.Until(sess.ExpiresAt) < sessionLifetime-24*time.Hour {
		if err := s.pool.QueryRow(ctx,
			`UPDATE sessions SET expires_at = now() + $2::interval WHERE id = $1 RETURNING expires_at`,
			sess.ID, sessionLifetime.String()).Scan(&sess.ExpiresAt); err != nil {
			return User{}, Session{}, err
		}
	}
	return u, sess, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

// VerifyEmail spends a verification token and marks the email verified, in one statement.
func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	tag, err := s.pool.Exec(ctx,
		`WITH t AS (
		   DELETE FROM email_tokens WHERE token_hash = $1 AND purpose = 'verify' AND expires_at > now()
		   RETURNING user_id)
		 UPDATE users SET email_verified_at = coalesce(email_verified_at, now()), updated_at = now()
		   FROM t WHERE users.id = t.user_id`, hashToken(token))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBadToken
	}
	return nil
}

// reauthenticate checks the signed-in user's current password before a
// sensitive change, so a stolen session token alone can't make it. A wrong
// password counts toward the login lockout. It returns the user's email.
func (s *Service) reauthenticate(ctx context.Context, userID uuid.UUID, password string) (string, error) {
	var email string
	var hash *string
	var locked bool
	if err := s.pool.QueryRow(ctx,
		`SELECT email, password_hash, coalesce(locked_until > now(), false) FROM users WHERE id = $1`, userID).
		Scan(&email, &hash, &locked); err != nil {
		return "", err
	}
	if !s.emLimit.allow(email, time.Now()) {
		return "", ErrRateLimited
	}
	if locked {
		return "", ErrLocked
	}
	if hash == nil {
		return "", ErrBadCredentials
	}
	ok, err := checkPassword(ctx, *hash, password)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", s.recordFailure(ctx, userID)
	}
	return email, nil
}
