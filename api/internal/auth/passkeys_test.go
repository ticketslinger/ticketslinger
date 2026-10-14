package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

var testRP = virtualwebauthn.RelyingParty{Name: "TicketSlinger", ID: "localhost", Origin: "http://localhost:5173"}

func newSvcPool(t *testing.T) (*auth.Service, *pgxpool.Pool) {
	pool := testdb.New(t)
	return auth.NewService(pool, &mailer.Memory{}, auth.Options{RPID: "localhost", Origin: "http://localhost:5173", SecretKey: testKey}), pool
}

// device is a virtual authenticator holding one credential.
type device struct {
	authr virtualwebauthn.Authenticator
	cred  virtualwebauthn.Credential
}

func newDevice(userID uuid.UUID) *device {
	return &device{
		authr: virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: userID[:]}),
		cred:  virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2),
	}
}

func (d *device) attest(t *testing.T, opts any) []byte {
	t.Helper()
	b, _ := json.Marshal(opts)
	o, err := virtualwebauthn.ParseAttestationOptions(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(virtualwebauthn.CreateAttestationResponse(testRP, d.authr, d.cred, *o))
}

func (d *device) assert(t *testing.T, opts any) []byte {
	t.Helper()
	b, _ := json.Marshal(opts)
	o, err := virtualwebauthn.ParseAssertionOptions(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(virtualwebauthn.CreateAssertionResponse(testRP, d.authr, d.cred, *o))
}

func signup(t *testing.T, s *auth.Service, email string) auth.User {
	t.Helper()
	_, u, err := s.Signup(context.Background(), email, "a long passphrase", "A", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// register adds d's credential to u's account, re-authenticating by password.
func register(t *testing.T, s *auth.Service, u auth.User, d *device) {
	t.Helper()
	ctx := context.Background()
	id, opts, err := s.BeginPasskeyRegistration(ctx, u.ID, false, "a long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishPasskeyRegistration(ctx, u.ID, id, d.attest(t, opts), "Laptop"); err != nil {
		t.Fatal(err)
	}
}

func TestPasskeyRegisterThenLogin(t *testing.T) {
	s, pool := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")
	d := newDevice(u.ID)

	id, opts, err := s.BeginPasskeyRegistration(ctx, u.ID, false, "a long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	pk, err := s.FinishPasskeyRegistration(ctx, u.ID, id, d.attest(t, opts), "  Laptop ")
	if err != nil || pk.Name != "Laptop" || pk.ID == uuid.Nil {
		t.Fatalf("register: %+v %v", pk, err)
	}

	lid, lopts, err := s.BeginPasskeyLogin(ctx, "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	sess, got, err := s.FinishPasskeyLogin(ctx, lid, d.assert(t, lopts), "203.0.113.1", "test")
	if err != nil || got.ID != u.ID || !sess.MFA || sess.Token == "" {
		t.Fatalf("login: user=%v mfa=%v err=%v", got.ID, sess.MFA, err)
	}
	if _, as, err := s.Authenticate(ctx, sess.Token); err != nil || !as.MFA {
		t.Fatalf("authenticate: %+v %v", as, err)
	}
	var used bool
	if err := pool.QueryRow(ctx, `SELECT last_used_at IS NOT NULL FROM passkeys WHERE id = $1`, pk.ID).Scan(&used); err != nil || !used {
		t.Fatalf("last_used_at not set: %v %v", used, err)
	}
}

func TestPasskeyNameDefaultsAndIsBounded(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")
	d := newDevice(u.ID)
	id, opts, _ := s.BeginPasskeyRegistration(ctx, u.ID, true, "")
	if pk, err := s.FinishPasskeyRegistration(ctx, u.ID, id, d.attest(t, opts), ""); err != nil || pk.Name != "Passkey" {
		t.Fatalf("default name: %+v %v", pk, err)
	}
	id, opts, _ = s.BeginPasskeyRegistration(ctx, u.ID, true, "")
	long := string(make([]rune, 121))
	if _, err := s.FinishPasskeyRegistration(ctx, u.ID, id, newDevice(u.ID).attest(t, opts), long); !errors.Is(err, auth.ErrInvalidName) {
		t.Fatalf("long name: got %v, want ErrInvalidName", err)
	}
}

func TestPasskeyChallengeIsSingleUse(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")
	d := newDevice(u.ID)
	register(t, s, u, d)

	lid, lopts, _ := s.BeginPasskeyLogin(ctx, "")
	asr := d.assert(t, lopts)
	if _, _, err := s.FinishPasskeyLogin(ctx, lid, asr, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishPasskeyLogin(ctx, lid, asr, "", ""); !errors.Is(err, auth.ErrBadToken) {
		t.Fatalf("second use of a challenge: got %v, want ErrBadToken", err)
	}
	// A failed attempt spends the challenge too.
	lid, _, _ = s.BeginPasskeyLogin(ctx, "")
	_, _, _ = s.FinishPasskeyLogin(ctx, lid, []byte("{}"), "", "")
	if _, _, err := s.FinishPasskeyLogin(ctx, lid, []byte("{}"), "", ""); !errors.Is(err, auth.ErrBadToken) {
		t.Fatalf("reuse after failure: got %v, want ErrBadToken", err)
	}
}

func TestPasskeyChallengeKindOwnerAndExpiry(t *testing.T) {
	s, pool := newSvcPool(t)
	ctx := context.Background()
	a := signup(t, s, "a@example.com")
	b := signup(t, s, "b@example.com")

	// A login challenge can't finish a registration, nor the reverse.
	lid, _, _ := s.BeginPasskeyLogin(ctx, "")
	if _, err := s.FinishPasskeyRegistration(ctx, a.ID, lid, []byte("{}"), ""); !errors.Is(err, auth.ErrBadToken) {
		t.Fatalf("login challenge used to register: %v", err)
	}
	rid, _, _ := s.BeginPasskeyRegistration(ctx, a.ID, true, "")
	if _, _, err := s.FinishPasskeyLogin(ctx, rid, []byte("{}"), "", ""); !errors.Is(err, auth.ErrBadToken) {
		t.Fatalf("register challenge used to log in: %v", err)
	}

	// B can't finish A's registration.
	rid, opts, _ := s.BeginPasskeyRegistration(ctx, a.ID, true, "")
	if _, err := s.FinishPasskeyRegistration(ctx, b.ID, rid, newDevice(b.ID).attest(t, opts), ""); !errors.Is(err, auth.ErrBadToken) {
		t.Fatalf("other user's challenge: %v", err)
	}

	// An expired challenge is refused.
	rid, opts, _ = s.BeginPasskeyRegistration(ctx, a.ID, true, "")
	if _, err := pool.Exec(ctx, `UPDATE webauthn_challenges SET expires_at = now() - interval '1 second' WHERE id = $1`, rid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishPasskeyRegistration(ctx, a.ID, rid, newDevice(a.ID).attest(t, opts), ""); !errors.Is(err, auth.ErrBadToken) {
		t.Fatalf("expired challenge: %v", err)
	}
	var exp time.Time
	_, _, _ = s.BeginPasskeyLogin(ctx, "")
	if err := pool.QueryRow(ctx, `SELECT max(expires_at) FROM webauthn_challenges`).Scan(&exp); err != nil || time.Until(exp) > 5*time.Minute+time.Second || time.Until(exp) < 4*time.Minute {
		t.Fatalf("challenge expiry %v, %v", exp, err)
	}
}

func TestPasskeyRegistrationNeedsReauthentication(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")
	if _, _, err := s.BeginPasskeyRegistration(ctx, u.ID, false, ""); !errors.Is(err, auth.ErrPasswordRequired) {
		t.Fatalf("no password: got %v, want ErrPasswordRequired", err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := s.BeginPasskeyRegistration(ctx, u.ID, false, "wrong password!"); !errors.Is(err, auth.ErrBadCredentials) {
			t.Fatalf("wrong password: got %v, want ErrBadCredentials", err)
		}
	}
	// The wrong passwords count toward the login lockout.
	if _, _, err := s.Login(ctx, "a@example.com", "a long passphrase", "", "", ""); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("login after 5 wrong passwords: got %v, want ErrLocked", err)
	}
	if _, _, err := s.BeginPasskeyRegistration(ctx, u.ID, false, "a long passphrase"); !errors.Is(err, auth.ErrLocked) {
		t.Fatalf("locked account: got %v, want ErrLocked", err)
	}
	// An mfa session needs no password, even while locked out of password sign-in.
	if _, _, err := s.BeginPasskeyRegistration(ctx, u.ID, true, ""); err != nil {
		t.Fatalf("mfa session: %v", err)
	}
}

func TestPasskeyCloneWarningRefusesLogin(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")
	d := newDevice(u.ID)
	d.cred.Counter = 5
	register(t, s, u, d)

	d.cred.Counter = 6
	lid, lopts, _ := s.BeginPasskeyLogin(ctx, "")
	if _, _, err := s.FinishPasskeyLogin(ctx, lid, d.assert(t, lopts), "", ""); err != nil {
		t.Fatalf("counter forward: %v", err)
	}
	d.cred.Counter = 6 // a copy of the key replaying an old count
	lid, lopts, _ = s.BeginPasskeyLogin(ctx, "")
	if _, _, err := s.FinishPasskeyLogin(ctx, lid, d.assert(t, lopts), "", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("counter not advancing: got %v, want ErrBadCredentials", err)
	}
}

func TestPasskeyLoginUnknownCredential(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")
	register(t, s, u, newDevice(u.ID))
	lid, lopts, _ := s.BeginPasskeyLogin(ctx, "")
	if _, _, err := s.FinishPasskeyLogin(ctx, lid, newDevice(u.ID).assert(t, lopts), "", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("unregistered key: got %v, want ErrBadCredentials", err)
	}
}

func TestPasskeyLoginIsRateLimitedPerIP(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if _, _, err := s.BeginPasskeyLogin(ctx, "203.0.113.9"); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, _, err := s.BeginPasskeyLogin(ctx, "203.0.113.9"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("21st begin: got %v, want ErrRateLimited", err)
	}
	if _, _, err := s.FinishPasskeyLogin(ctx, uuid.New(), []byte("{}"), "203.0.113.9", ""); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("finish from the same IP: got %v, want ErrRateLimited", err)
	}
	if _, _, err := s.BeginPasskeyLogin(ctx, "203.0.113.10"); err != nil {
		t.Fatalf("another IP: %v", err)
	}
}

// The mfa=true claim rests on user verification and on these option sets.
func TestPasskeyOptionsRequireResidentKeyAndUV(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")
	_, ropts, err := s.BeginPasskeyRegistration(ctx, u.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		PublicKey struct {
			AuthenticatorSelection struct{ ResidentKey, UserVerification string }
		}
	}
	b, _ := json.Marshal(ropts)
	_ = json.Unmarshal(b, &reg)
	if sel := reg.PublicKey.AuthenticatorSelection; sel.ResidentKey != "required" || sel.UserVerification != "required" {
		t.Fatalf("registration authenticatorSelection = %+v in %s", sel, b)
	}
	_, lopts, err := s.BeginPasskeyLogin(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		PublicKey struct{ UserVerification string }
	}
	b, _ = json.Marshal(lopts)
	_ = json.Unmarshal(b, &login)
	if login.PublicKey.UserVerification != "required" {
		t.Fatalf("login userVerification = %q in %s", login.PublicKey.UserVerification, b)
	}
}

func TestPasskeyWithoutUserVerificationIsRefused(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")

	noUV := newDevice(u.ID)
	noUV.authr.Options.UserNotVerified = true
	id, opts, _ := s.BeginPasskeyRegistration(ctx, u.ID, true, "")
	if _, err := s.FinishPasskeyRegistration(ctx, u.ID, id, noUV.attest(t, opts), ""); !errors.Is(err, auth.ErrBadPasskey) {
		t.Fatalf("registration without UV: got %v, want ErrBadPasskey", err)
	}

	d := newDevice(u.ID)
	register(t, s, u, d)
	d.authr.Options.UserNotVerified = true
	lid, lopts, _ := s.BeginPasskeyLogin(ctx, "")
	if _, _, err := s.FinishPasskeyLogin(ctx, lid, d.assert(t, lopts), "", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("login without UV: got %v, want ErrBadCredentials", err)
	}
}

func TestPasskeyLoginFromAnotherOriginIsRefused(t *testing.T) {
	s, _ := newSvcPool(t)
	ctx := context.Background()
	u := signup(t, s, "a@example.com")
	d := newDevice(u.ID)
	register(t, s, u, d)

	lid, lopts, _ := s.BeginPasskeyLogin(ctx, "")
	b, _ := json.Marshal(lopts)
	o, err := virtualwebauthn.ParseAssertionOptions(string(b))
	if err != nil {
		t.Fatal(err)
	}
	evil := virtualwebauthn.RelyingParty{Name: "TicketSlinger", ID: "localhost", Origin: "https://evil.example"}
	asr := virtualwebauthn.CreateAssertionResponse(evil, d.authr, d.cred, *o)
	if _, _, err := s.FinishPasskeyLogin(ctx, lid, []byte(asr), "", ""); !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("assertion from another origin: got %v, want ErrBadCredentials", err)
	}
}
