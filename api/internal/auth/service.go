// Package auth holds identity services: passwords, sessions and email verification.
package auth

import (
	"context"
	"crypto/hmac"
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
	"github.com/ticketslinger/ticketslinger/api/internal/ratelimit"
	"github.com/ticketslinger/ticketslinger/api/internal/turnstile"
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
	// ErrTurnstileRequired: this sign-in needs a Turnstile token first.
	ErrTurnstileRequired = errors.New("auth: turnstile required")
	// ErrThrottled: with Turnstile off, an account past accountBrake takes
	// one sign-in attempt per ThrottleEvery. Try again after it.
	ErrThrottled = errors.New("auth: throttled")
)

// ThrottleEvery is how often an account past accountBrake may try to sign
// in when Turnstile is off: from all addresses together, and never a lock.
const ThrottleEvery = 10 * time.Second

const (
	minPassword     = 12
	maxPassword     = 256
	maxName         = 120
	sessionLifetime = 30 * 24 * time.Hour
	verifyLifetime  = 48 * time.Hour
	maxSessionAge   = 90 * 24 * time.Hour // absolute cap, however often a session slides
	// Sign-in attempts count per email and address (see ratelimit.AddrKey) over a
	// window that restarts with each one; a success clears its address.
	// After turnstileAfter, that address needs Turnstile; the maxFailures-th
	// locks it out for the window. accountBrake over all addresses puts every
	// address behind Turnstile, and never locks, so a stranger can't lock
	// the owner out. Emails without an account count the same way.
	turnstileAfter = 3
	maxFailures    = 5
	accountBrake   = 10
	lockout        = 15 * time.Minute
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
	// Turnstile guards sign-up and repeated failed sign-ins; nil turns it off.
	Turnstile *turnstile.Verifier
}

type Service struct {
	pool    *pgxpool.Pool
	mail    mailer.Mailer
	opts    Options
	ipLimit *ratelimit.Limiter
	emLimit *ratelimit.Limiter
	// recLimit caps recovery-code generation per user; each call costs ten Argon2 hashes.
	recLimit   *ratelimit.Limiter
	beforeHash func()             // test hook, nil in production
	wa         *webauthn.WebAuthn // nil when RPID or Origin is unusable; passkey calls then return waErr
	waErr      error
	failKey    []byte // HMAC key that names an email in login_failures
	// throttle spaces attempts on a braked account when Turnstile is off.
	throttle *ratelimit.Limiter
	now      func() time.Time // test hook, nil in production
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func NewService(pool *pgxpool.Pool, m mailer.Mailer, o Options) *Service {
	s := &Service{pool: pool, mail: m, opts: o}
	s.ipLimit, s.emLimit, s.recLimit = newLimits()
	s.throttle = ratelimit.New(1, ThrottleEvery)
	s.failKey = DeriveKey(o.SecretKey, "login-failures")
	s.wa, s.waErr = newWebAuthn(o)
	return s
}

// DeriveKey is a key for one purpose, named by label, derived from
// TS_SECRET_KEY: HMAC-SHA256(secret, label). Each use gets its own label, so
// no two uses ever share a key, and none uses the secret itself.
func DeriveKey(secret []byte, label string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

func newLimits() (ip, email, recovery *ratelimit.Limiter) {
	return ratelimit.New(20, time.Minute), ratelimit.New(10, time.Minute), ratelimit.New(5, time.Hour)
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
	if strings.HasPrefix(tok, "tsk_") { // API keys own that prefix (1 in 16.7M)
		return newToken()
	}
	return tok, hashToken(tok), nil
}

func hashToken(tok string) []byte { h := sha256.Sum256([]byte(tok)); return h[:] }

// Signup creates an account. With Turnstile on, turnstileToken must pass
// (turnstile.ErrFailed, or turnstile.ErrUnavailable when Cloudflare can't say).
func (s *Service) Signup(ctx context.Context, email, password, name, turnstileToken, ip, ua string) (Session, User, error) {
	if !s.ipLimit.Allow("signup:"+ratelimit.AddrKey(ip), time.Now()) {
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
	if err := s.opts.Turnstile.Verify(ctx, turnstileToken, ip); err != nil {
		return Session{}, User{}, err
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

// Login signs in with a password and, when the account has one, a second
// step. Every attempt counts until one succeeds: after a few from one
// address it needs turnstileToken (ErrTurnstileRequired), then that address
// is locked out (ErrLocked). An email with no account answers the same way.
func (s *Service) Login(ctx context.Context, email, password, totpCode, recoveryCode, turnstileToken, ip, ua string) (Session, User, error) {
	if totpCode != "" && recoveryCode != "" {
		return Session{}, User{}, ErrOneSecondStep
	}
	email = normEmail(email)
	now := s.clock()
	// Per email and address: a stranger hammering one address mustn't
	// rate-limit the owner everywhere. Spread-out guessing meets accountBrake.
	if !s.ipLimit.Allow("login:"+ratelimit.AddrKey(ip), now) || !s.emLimit.Allow(email+"|"+ratelimit.AddrKey(ip), now) {
		return Session{}, User{}, ErrRateLimited
	}
	account := s.accountKey(email)
	// Turnstile off: past the brake, the account takes one attempt per
	// ThrottleEvery from everywhere. Checked before counting, so a refused
	// attempt never moves anyone toward a lock. Same query for any email.
	if !s.opts.Turnstile.Enabled() {
		braked, err := s.braked(ctx, account)
		if err != nil {
			return Session{}, User{}, err
		}
		if braked && !s.throttle.Allow(string(account), now) {
			return Session{}, User{}, ErrThrottled
		}
	}
	// Counted before anything is checked, whether or not the account exists.
	here, total, err := s.reserve(ctx, account, ip)
	if err != nil {
		return Session{}, User{}, err
	}
	if s.opts.Turnstile.Enabled() && (here > turnstileAfter || total >= accountBrake) {
		if turnstileToken == "" {
			return Session{}, User{}, ErrTurnstileRequired
		}
		if err := s.opts.Turnstile.Verify(ctx, turnstileToken, ip); err != nil {
			return Session{}, User{}, err
		}
	}
	var (
		u      User
		hash   *string
		totpOn bool
		sealed []byte
		last   *int64
		hasKey bool
	)
	err = s.pool.QueryRow(ctx,
		`SELECT id, email, name, email_verified_at IS NOT NULL, totp_enabled_at IS NOT NULL,
		        password_hash, totp_secret, totp_last_step,
		        EXISTS (SELECT 1 FROM passkeys p WHERE p.user_id = users.id)
		   FROM users WHERE email = $1`, email).
		Scan(&u.ID, &u.Email, &u.Name, &u.EmailVerified, &totpOn, &hash, &sealed, &last, &hasKey)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hash == nil) {
		_, _ = checkPassword(ctx, dummyHash(), password)
		return Session{}, User{}, ErrBadCredentials
	}
	if err != nil {
		return Session{}, User{}, err
	}
	u.TOTPEnabled = totpOn
	ok, err := checkPassword(ctx, *hash, password)
	if err != nil {
		return Session{}, User{}, err
	}
	if !ok {
		return Session{}, User{}, ErrBadCredentials
	}
	var codeID uuid.UUID
	if recoveryCode != "" {
		if !totpOn && !hasKey { // stale codes from a factor since removed
			return Session{}, User{}, ErrBadCredentials
		}
		// Accepted for any account with a second factor; it stands in for the TOTP code or the passkey.
		if codeID, err = s.matchRecovery(ctx, u.ID, recoveryCode); err != nil {
			return Session{}, User{}, err
		}
		if codeID == uuid.Nil {
			return Session{}, User{}, ErrBadCredentials
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
			return Session{}, User{}, ErrBadCredentials
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
		if _, err := tx.Exec(ctx, `DELETE FROM login_failures WHERE account = $1 AND ip = $2`, account, ratelimit.AddrKey(ip)); err != nil {
			return err
		}
		sess, err = createSession(ctx, tx, u.ID, totpOn || codeID != uuid.Nil, ip, ua)
		return err
	})
	if err == nil && replay {
		return Session{}, User{}, ErrBadCredentials
	}
	return sess, u, err
}

// accountKey names email in login_failures: an HMAC, so the table can't be
// read back to emails with a dictionary.
func (s *Service) accountKey(email string) []byte {
	mac := hmac.New(sha256.New, s.failKey)
	mac.Write([]byte(normEmail(email)))
	return mac.Sum(nil)
}

// braked reports whether the account has accountBrake failures in the
// window, over all addresses.
func (s *Service) braked(ctx context.Context, account []byte) (bool, error) {
	var total int
	err := s.pool.QueryRow(ctx, `SELECT coalesce(sum(failures), 0) FROM login_failures
		WHERE account = $1 AND updated_at > now() - $2::interval`, account, lockout.String()).Scan(&total)
	return total >= accountBrake, err
}

// reserve counts this attempt from ip before any check runs, so parallel
// guesses can't race past the limits. It returns this address's attempts in
// the window, this one included, and the account's over all addresses
// before this one. A locked address gets ErrLocked and isn't counted. A
// count lapses once lockout passes without an attempt; the sweep deletes
// the row a day later.
func (s *Service) reserve(ctx context.Context, account []byte, ip string) (here, total int, err error) {
	err = s.pool.QueryRow(ctx,
		`WITH r AS (
		   INSERT INTO login_failures AS f (account, ip, failures) VALUES ($1, $2, 1)
		   ON CONFLICT (account, ip) DO UPDATE SET
		     failures     = CASE WHEN f.updated_at > now() - $3::interval THEN f.failures + 1 ELSE 1 END,
		     locked_until = CASE WHEN f.updated_at > now() - $3::interval AND f.failures + 1 >= $4 THEN now() + $3::interval END,
		     updated_at   = now()
		   WHERE f.locked_until IS NULL OR f.locked_until <= now()
		   RETURNING failures)
		 SELECT r.failures, (SELECT coalesce(sum(failures), 0) FROM login_failures
		                      WHERE account = $1 AND updated_at > now() - $3::interval)
		   FROM r`,
		account, ratelimit.AddrKey(ip), lockout.String(), maxFailures).Scan(&here, &total)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrLocked
	}
	return here, total, err
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
// password counts toward the sign-in lockout for ip. It returns the user's email.
func (s *Service) reauthenticate(ctx context.Context, userID uuid.UUID, password, ip string) (string, error) {
	var email string
	var hash *string
	if err := s.pool.QueryRow(ctx, `SELECT email, password_hash FROM users WHERE id = $1`, userID).
		Scan(&email, &hash); err != nil {
		return "", err
	}
	if !s.emLimit.Allow(normEmail(email)+"|"+ratelimit.AddrKey(ip), time.Now()) {
		return "", ErrRateLimited
	}
	account := s.accountKey(email)
	if _, _, err := s.reserve(ctx, account, ip); err != nil {
		return "", err
	}
	if hash == nil {
		return "", ErrBadCredentials
	}
	ok, err := checkPassword(ctx, *hash, password)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrBadCredentials
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM login_failures WHERE account = $1 AND ip = $2`, account, ratelimit.AddrKey(ip))
	return email, err
}
