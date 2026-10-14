package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrBadPasskey means a registration response didn't verify.
var ErrBadPasskey = errors.New("auth: bad passkey")

const challengeLifetime = 5 * time.Minute

type Passkey struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
}

func newWebAuthn(o Options) (*webauthn.WebAuthn, error) {
	yes := true
	return webauthn.New(&webauthn.Config{
		RPID:          o.RPID,
		RPDisplayName: "TicketSlinger",
		RPOrigins:     []string{o.Origin},
		// Discoverable credentials with user verification: the passkey alone
		// signs someone in, so it has to be both findable and a second factor.
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: &yes,
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: challengeLifetime},
			Registration: webauthn.TimeoutConfig{Timeout: challengeLifetime},
		},
	})
}

// waUser adapts a user and their stored passkeys to webauthn.User.
type waUser struct {
	User
	creds []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.ID[:] }
func (u *waUser) WebAuthnName() string                       { return u.Email }
func (u *waUser) WebAuthnDisplayName() string                { return u.Name }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (s *Service) loadWebAuthnUser(ctx context.Context, id uuid.UUID) (*waUser, error) {
	u := &waUser{}
	if err := s.pool.QueryRow(ctx,
		`SELECT id, email, name, email_verified_at IS NOT NULL, totp_enabled_at IS NOT NULL FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Email, &u.Name, &u.EmailVerified, &u.TOTPEnabled); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT credential FROM passkeys WHERE user_id = $1`, id)
	if err != nil {
		return nil, err
	}
	u.creds, err = pgx.CollectRows(rows, pgx.RowTo[webauthn.Credential])
	return u, err
}

// saveChallenge stores ceremony state server-side and clears out expired
// challenges, so abandoned ceremonies don't pile up.
func (s *Service) saveChallenge(ctx context.Context, userID *uuid.UUID, kind string, sd *webauthn.SessionData) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`WITH gone AS (DELETE FROM webauthn_challenges WHERE expires_at <= now())
		 INSERT INTO webauthn_challenges (user_id, kind, data, expires_at)
		 VALUES ($1, $2, $3, now() + $4::interval) RETURNING id`,
		userID, kind, sd, challengeLifetime.String()).Scan(&id)
	return id, err
}

// consumeChallenge deletes and returns a live challenge of the given kind
// belonging to userID (nil for a login). Deleting first makes every challenge
// single-use, whether or not the ceremony then succeeds.
func (s *Service) consumeChallenge(ctx context.Context, id uuid.UUID, kind string, userID *uuid.UUID) (webauthn.SessionData, error) {
	var sd webauthn.SessionData
	err := s.pool.QueryRow(ctx,
		`DELETE FROM webauthn_challenges
		  WHERE id = $1 AND kind = $2 AND expires_at > now() AND user_id IS NOT DISTINCT FROM $3
		 RETURNING data`, id, kind, userID).Scan(&sd)
	if errors.Is(err, pgx.ErrNoRows) {
		return sd, ErrBadToken
	}
	return sd, err
}

// BeginPasskeyRegistration starts adding a passkey to the signed-in user.
// Once the account has a second factor, only a session that passed one may
// add another (see checkEnrolment). The first passkey on a password session
// needs the current password, so a stolen session token can't plant one.
func (s *Service) BeginPasskeyRegistration(ctx context.Context, userID uuid.UUID, mfa bool, password, ip string) (uuid.UUID, *protocol.CredentialCreation, error) {
	if s.waErr != nil {
		return uuid.Nil, nil, s.waErr
	}
	if err := s.checkEnrolment(ctx, userID, mfa); err != nil {
		return uuid.Nil, nil, err
	}
	if !mfa {
		if password == "" {
			return uuid.Nil, nil, ErrPasswordRequired
		}
		if _, err := s.reauthenticate(ctx, userID, password, ip); err != nil {
			return uuid.Nil, nil, err
		}
	}
	u, err := s.loadWebAuthnUser(ctx, userID)
	if err != nil {
		return uuid.Nil, nil, err
	}
	creation, sd, err := s.wa.BeginRegistration(u,
		webauthn.WithExclusions(webauthn.Credentials(u.creds).CredentialDescriptors()))
	if err != nil {
		return uuid.Nil, nil, err
	}
	id, err := s.saveChallenge(ctx, &userID, "register", sd)
	return id, creation, err
}

// checkEnrolment refuses to add a second factor from a session that hasn't
// passed one when the account already has a passkey or an authenticator app.
// A password alone would otherwise let whoever knows it enrol their own
// factor and then sign in with it as mfa. Starting a ceremony checks it
// early; saving the factor checks it again under the same lock.
func (s *Service) checkEnrolment(ctx context.Context, userID uuid.UUID, mfa bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return lockEnrolment(ctx, tx, userID, mfa) })
}

// lockEnrolment locks the user's row for the rest of tx, then applies the
// checkEnrolment rule. Every enrolment takes this lock before adding a
// factor, so two racing enrolments serialise and the second sees the first.
// The lock and the look are separate statements: under READ COMMITTED the
// second one gets a fresh snapshot that includes what the lock holder committed.
func lockEnrolment(ctx context.Context, tx pgx.Tx, userID uuid.UUID, mfa bool) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return err
	}
	if mfa {
		return nil
	}
	var has bool
	if err := tx.QueryRow(ctx,
		`SELECT totp_enabled_at IS NOT NULL OR EXISTS (SELECT 1 FROM passkeys WHERE user_id = $1)
		   FROM users WHERE id = $1`, userID).Scan(&has); err != nil {
		return err
	}
	if has {
		return ErrMFARequired
	}
	return nil
}

// FinishPasskeyRegistration verifies the authenticator's attestation (the
// JSON body from navigator.credentials.create) and stores the passkey, once
// checkEnrolment's rule still holds under the user's row lock.
func (s *Service) FinishPasskeyRegistration(ctx context.Context, userID uuid.UUID, mfa bool, challengeID uuid.UUID, body []byte, name string) (Passkey, error) {
	if s.waErr != nil {
		return Passkey{}, s.waErr
	}
	if name = strings.TrimSpace(name); name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > maxName {
		return Passkey{}, ErrInvalidName
	}
	sd, err := s.consumeChallenge(ctx, challengeID, "register", &userID)
	if err != nil {
		return Passkey{}, err
	}
	u, err := s.loadWebAuthnUser(ctx, userID)
	if err != nil {
		return Passkey{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		slog.InfoContext(ctx, "passkey registration unreadable", "user", userID, "err", err)
		return Passkey{}, ErrBadPasskey
	}
	cred, err := s.wa.CreateCredential(u, sd, parsed)
	if err != nil {
		slog.InfoContext(ctx, "passkey registration rejected", "user", userID, "err", err)
		return Passkey{}, ErrBadPasskey
	}
	pk := Passkey{}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockEnrolment(ctx, tx, userID, mfa); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO passkeys (user_id, credential_id, credential, name) VALUES ($1, $2, $3, $4)
			 RETURNING id, name, created_at`, userID, cred.ID, cred, name).Scan(&pk.ID, &pk.Name, &pk.CreatedAt)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Passkey{}, ErrBadPasskey // already registered
	}
	return pk, err
}

// BeginPasskeyLogin starts a sign-in with a discoverable credential; the
// authenticator, not the caller, says who is signing in.
func (s *Service) BeginPasskeyLogin(ctx context.Context, ip string) (uuid.UUID, *protocol.CredentialAssertion, error) {
	if s.waErr != nil {
		return uuid.Nil, nil, s.waErr
	}
	if !s.ipLimit.Allow("passkey:"+ip, time.Now()) {
		return uuid.Nil, nil, ErrRateLimited
	}
	assertion, sd, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		return uuid.Nil, nil, err
	}
	id, err := s.saveChallenge(ctx, nil, "login", sd)
	return id, assertion, err
}

// FinishPasskeyLogin verifies the assertion (the JSON body from
// navigator.credentials.get) and creates a session. A passkey is possession
// plus user verification, so the session counts as mfa.
func (s *Service) FinishPasskeyLogin(ctx context.Context, challengeID uuid.UUID, body []byte, ip, ua string) (Session, User, error) {
	if s.waErr != nil {
		return Session{}, User{}, s.waErr
	}
	if !s.ipLimit.Allow("passkey:"+ip, time.Now()) {
		return Session{}, User{}, ErrRateLimited
	}
	sd, err := s.consumeChallenge(ctx, challengeID, "login", nil)
	if err != nil {
		return Session{}, User{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		slog.InfoContext(ctx, "passkey assertion unreadable", "err", err)
		return Session{}, User{}, ErrBadCredentials
	}
	var (
		owner   *waUser
		loadErr error
	)
	_, cred, err := s.wa.ValidatePasskeyLogin(func(_, handle []byte) (webauthn.User, error) {
		id, err := uuid.FromBytes(handle)
		if err != nil {
			return nil, err
		}
		owner, loadErr = s.loadWebAuthnUser(ctx, id)
		return owner, loadErr
	}, sd, parsed)
	if loadErr != nil && !errors.Is(loadErr, pgx.ErrNoRows) {
		return Session{}, User{}, loadErr
	}
	if err != nil {
		slog.InfoContext(ctx, "passkey assertion rejected", "err", err)
		return Session{}, User{}, ErrBadCredentials
	}
	if cred.Authenticator.CloneWarning {
		slog.WarnContext(ctx, "passkey sign count did not advance; possible cloned authenticator", "user", owner.ID)
		return Session{}, User{}, ErrBadCredentials
	}
	var sess Session
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The stored count must still be below the new one: of two sign-ins
		// racing with one counter value, only one gets the row.
		tag, err := tx.Exec(ctx,
			`UPDATE passkeys SET credential = $3, last_used_at = now()
			  WHERE credential_id = $1 AND user_id = $2
			    AND ($4::bigint = 0 OR coalesce((credential #>> '{authenticator,signCount}')::bigint, 0) < $4::bigint)`,
			cred.ID, owner.ID, cred, int64(cred.Authenticator.SignCount))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrBadCredentials
		}
		sess, err = createSession(ctx, tx, owner.ID, true, ip, ua)
		return err
	})
	if err != nil {
		return Session{}, User{}, err
	}
	return sess, owner.User, nil
}
