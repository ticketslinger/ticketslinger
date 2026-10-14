package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/apikeys"
	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
)

type Deps struct {
	Pool       *pgxpool.Pool
	Auth       *auth.Service
	Organizers *organizers.Service
	Keys       *apikeys.Service
	Config     config.Config
}

type server struct {
	Deps
	public map[string]bool // mux patterns ("POST /v1/auth/login") whose spec says security: []
}

func New(d Deps) http.Handler {
	if d.Organizers == nil {
		d.Organizers = organizers.New(d.Pool)
	}
	if d.Keys == nil {
		d.Keys = apikeys.New(d.Pool)
	}
	s := &server{Deps: d, public: publicRoutes()}
	strict := NewStrictHandlerWithOptions(s, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  func(w http.ResponseWriter, r *http.Request, err error) { writeBadRequest(w, r, err) },
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) { writeError(w, err) },
	})
	mux := http.NewServeMux()
	HandlerWithOptions(strict, StdHTTPServerOptions{
		BaseURL:    "/v1",
		BaseRouter: mux,
		// Runs before the strict handler decodes the body, so an
		// unauthenticated request is a 401 whatever it sends.
		Middlewares: []MiddlewareFunc{s.authenticate},
		// Path-parameter parse failures from the generated wrapper.
		ErrorHandlerFunc: writeBadRequest,
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeError(w, notFound()) })
	return requestID(recoverer(limitBody(mux)))
}

// publicRoutes reads the embedded spec once and returns the mux patterns of
// operations that opt out of the global session requirement with an explicit
// `security: []`. Anything not listed needs a session.
func publicRoutes() map[string]bool {
	spec, err := GetSwagger()
	if err != nil {
		panic(err)
	}
	public := map[string]bool{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				public[method+" /v1"+path] = true
			}
		}
	}
	return public
}

type ctxKey int

const (
	clientKey ctxKey = iota
	authKey
)

type client struct{ ip, ua string }

// authed is the caller: a session (user, sess, token) or an API key (key).
// Never both.
type authed struct {
	user  auth.User
	sess  auth.Session
	token string
	key   db.Principal
}

func (a authed) isKey() bool { return a.key.KeyID != uuid.Nil }

// authenticate records the caller's address and, for every route that
// isn't public, requires a valid bearer session or API key. r.Pattern is the
// mux pattern the generated wrapper registered; a route not found in the
// public set needs a caller.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientKey, client{clientIP(r, s.Config.TrustedProxies), r.UserAgent()})
		if !s.public[r.Pattern] {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				writeError(w, unauthorized())
				return
			}
			var a authed
			var err error
			if strings.HasPrefix(token, apikeys.Prefix) {
				a.key, err = s.Keys.Authenticate(ctx, token)
			} else {
				a.token = token
				a.user, a.sess, err = s.Auth.Authenticate(ctx, token)
			}
			if err != nil {
				if errors.Is(err, auth.ErrBadToken) || errors.Is(err, apikeys.ErrBadKey) {
					err = unauthorized()
				}
				writeError(w, err)
				return
			}
			ctx = context.WithValue(ctx, authKey, a)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// clientIP is the peer's address, or X-Real-IP when the peer is a trusted
// proxy. X-Forwarded-For is ignored. Returns "" if nothing parses.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	peer := ap.Addr().Unmap().WithZone("")
	for _, p := range trusted {
		if p.Contains(peer) {
			if real, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
				return real.Unmap().WithZone("").String()
			}
			break
		}
	}
	return peer.String()
}

// errSessionRequired refuses an API key on anything that needs a person.
var errSessionRequired = &apiError{403, "session_required", "An API key can't do this. Sign in to the dashboard instead."}

// userFrom is the signed-in caller of a user-scoped operation. An API key
// gets errSessionRequired: it has no account to act on.
func userFrom(ctx context.Context) (auth.User, auth.Session, error) {
	a, ok := ctx.Value(authKey).(authed)
	if !ok {
		return auth.User{}, auth.Session{}, unauthorized()
	}
	if a.isKey() {
		return auth.User{}, auth.Session{}, errSessionRequired
	}
	return a.user, a.sess, nil
}

func clientFrom(ctx context.Context) client {
	c, _ := ctx.Value(clientKey).(client)
	return c
}

// limitBody caps every request body at 1 MiB.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", uuid.NewString())
		next.ServeHTTP(w, r)
	})
}

func (s *server) GetHealth(context.Context, GetHealthRequestObject) (GetHealthResponseObject, error) {
	return GetHealth200JSONResponse{Status: "ok"}, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func me(u auth.User, sess auth.Session) Me {
	return Me{Id: u.ID, Email: u.Email, Name: u.Name, EmailVerified: u.EmailVerified, TotpEnabled: u.TOTPEnabled, HasPasskey: u.HasPasskey, HasSecondFactor: u.HasPasskey || u.TOTPEnabled, Mfa: sess.MFA}
}

func (s *server) Signup(ctx context.Context, req SignupRequestObject) (SignupResponseObject, error) {
	c := clientFrom(ctx)
	sess, u, err := s.Auth.Signup(ctx, string(req.Body.Email), req.Body.Password, req.Body.Name, c.ip, c.ua)
	switch {
	case errors.Is(err, auth.ErrEmailTaken):
		return nil, &apiError{409, "email_taken", "There's already an account with that email. Try signing in instead."}
	case errors.Is(err, auth.ErrPasswordTooShort):
		return nil, &apiError{422, "password_too_short", "Your password needs at least 12 characters."}
	case errors.Is(err, auth.ErrPasswordTooLong):
		return nil, &apiError{422, "password_too_long", "Your password can be at most 256 characters."}
	case errors.Is(err, auth.ErrInvalidName):
		return nil, &apiError{422, "invalid_name", "Tell us your name, in 120 characters or fewer."}
	case errors.Is(err, auth.ErrRateLimited):
		return nil, tooManyAttempts()
	case err != nil:
		return nil, err
	}
	return Signup201JSONResponse{SessionToken: sess.Token, User: me(u, sess)}, nil
}

func (s *server) Login(ctx context.Context, req LoginRequestObject) (LoginResponseObject, error) {
	c := clientFrom(ctx)
	code := ""
	if req.Body.TotpCode != nil {
		code = *req.Body.TotpCode
	}
	recovery := ""
	if req.Body.RecoveryCode != nil {
		recovery = *req.Body.RecoveryCode
	}
	sess, u, err := s.Auth.Login(ctx, string(req.Body.Email), req.Body.Password, code, recovery, c.ip, c.ua)
	switch {
	case errors.Is(err, auth.ErrBadCredentials):
		return nil, &apiError{401, "bad_credentials", "That email and password don't match."}
	case errors.Is(err, auth.ErrTOTPRequired):
		return nil, &apiError{401, "totp_required", "Enter the 6-digit code from your authenticator app, or one of your recovery codes."}
	case errors.Is(err, auth.ErrOneSecondStep):
		return nil, &apiError{422, "one_second_step", "Use either the code from your authenticator app or a recovery code, not both."}
	case errors.Is(err, auth.ErrLocked):
		return nil, locked()
	case errors.Is(err, auth.ErrRateLimited):
		return nil, tooManyAttempts()
	case err != nil:
		return nil, err
	}
	return Login200JSONResponse{SessionToken: sess.Token, User: me(u, sess)}, nil
}

func (s *server) Logout(ctx context.Context, _ LogoutRequestObject) (LogoutResponseObject, error) {
	if _, _, err := userFrom(ctx); err != nil {
		return nil, err
	}
	a, _ := ctx.Value(authKey).(authed)
	if err := s.Auth.Logout(ctx, a.token); err != nil {
		return nil, err
	}
	return Logout204Response{}, nil
}

func (s *server) VerifyEmail(ctx context.Context, req VerifyEmailRequestObject) (VerifyEmailResponseObject, error) {
	err := s.Auth.VerifyEmail(ctx, req.Body.Token)
	if errors.Is(err, auth.ErrBadToken) {
		return nil, &apiError{400, "bad_token", "That link has expired or was already used. Sign in to get a new one."}
	}
	if err != nil {
		return nil, err
	}
	return VerifyEmail204Response{}, nil
}

func (s *server) GetMe(ctx context.Context, _ GetMeRequestObject) (GetMeResponseObject, error) {
	u, sess, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	return GetMe200JSONResponse(me(u, sess)), nil
}

// passkeyError maps passkey failures; badCreds is the message for
// ErrBadCredentials, which means a wrong password when adding a passkey and
// an unknown or refused passkey when signing in.
func passkeyError(err error, badCreds string) error {
	switch {
	case errors.Is(err, auth.ErrBadToken):
		return &apiError{400, "challenge_expired", "That passkey prompt expired or was already used. Start again."}
	case errors.Is(err, auth.ErrBadPasskey):
		return &apiError{400, "invalid_passkey", "We couldn't verify that passkey. Try again, or use a different one."}
	case errors.Is(err, auth.ErrInvalidName):
		return &apiError{422, "invalid_name", "Give your passkey a name of 120 characters or fewer."}
	case errors.Is(err, auth.ErrPasswordRequired):
		return &apiError{401, "password_required", "Enter your password to add a passkey."}
	case errors.Is(err, auth.ErrMFARequired):
		return errEnrolNeedsMFA
	case errors.Is(err, auth.ErrBadCredentials):
		return &apiError{401, "bad_credentials", badCreds}
	case errors.Is(err, auth.ErrLocked):
		return locked()
	case errors.Is(err, auth.ErrRateLimited):
		return tooManyAttempts()
	}
	return err
}

// toMap turns a WebAuthn options struct into the spec's free-form object.
func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func (s *server) BeginPasskeyRegistration(ctx context.Context, req BeginPasskeyRegistrationRequestObject) (BeginPasskeyRegistrationResponseObject, error) {
	u, sess, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	password := ""
	if req.Body != nil && req.Body.Password != nil {
		password = *req.Body.Password
	}
	id, opts, err := s.Auth.BeginPasskeyRegistration(ctx, u.ID, sess.MFA, password)
	if err != nil {
		return nil, passkeyError(err, "That password doesn't match.")
	}
	m, err := toMap(opts)
	if err != nil {
		return nil, err
	}
	return BeginPasskeyRegistration200JSONResponse{ChallengeId: id, Options: m}, nil
}

func (s *server) FinishPasskeyRegistration(ctx context.Context, req FinishPasskeyRegistrationRequestObject) (FinishPasskeyRegistrationResponseObject, error) {
	u, sess, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req.Body)
	if err != nil {
		return nil, err
	}
	name := ""
	if req.Params.Name != nil {
		name = *req.Params.Name
	}
	pk, err := s.Auth.FinishPasskeyRegistration(ctx, u.ID, sess.MFA, req.ChallengeId, body, name)
	if err != nil {
		return nil, passkeyError(err, "That password doesn't match.")
	}
	return FinishPasskeyRegistration201JSONResponse{Id: pk.ID, Name: pk.Name, CreatedAt: pk.CreatedAt}, nil
}

func (s *server) BeginPasskeyLogin(ctx context.Context, _ BeginPasskeyLoginRequestObject) (BeginPasskeyLoginResponseObject, error) {
	id, opts, err := s.Auth.BeginPasskeyLogin(ctx, clientFrom(ctx).ip)
	if err != nil {
		return nil, passkeyError(err, "")
	}
	m, err := toMap(opts)
	if err != nil {
		return nil, err
	}
	return BeginPasskeyLogin200JSONResponse{ChallengeId: id, Options: m}, nil
}

func (s *server) FinishPasskeyLogin(ctx context.Context, req FinishPasskeyLoginRequestObject) (FinishPasskeyLoginResponseObject, error) {
	c := clientFrom(ctx)
	body, err := json.Marshal(req.Body)
	if err != nil {
		return nil, err
	}
	sess, u, err := s.Auth.FinishPasskeyLogin(ctx, req.ChallengeId, body, c.ip, c.ua)
	if err != nil {
		return nil, passkeyError(err, "We couldn't sign you in with that passkey. Try another, or use your password.")
	}
	return FinishPasskeyLogin200JSONResponse{SessionToken: sess.Token, User: me(u, sess)}, nil
}

// errEnrolNeedsMFA refuses a new second factor from a session that hasn't
// passed the one the account already has.
var errEnrolNeedsMFA = &apiError{403, "mfa_required", "Sign in with your passkey or authenticator code first, then try again."}

func totpError(err error) error {
	switch {
	case errors.Is(err, auth.ErrTOTPAlreadyEnabled):
		return &apiError{409, "totp_already_enabled", "Two-factor sign-in is already switched on for your account."}
	case errors.Is(err, auth.ErrMFARequired):
		return errEnrolNeedsMFA
	case errors.Is(err, auth.ErrPasswordRequired):
		return &apiError{401, "password_required", "Enter your password to set up an authenticator app."}
	case errors.Is(err, auth.ErrTOTPNotStarted):
		return &apiError{400, "totp_not_started", "That setup has expired or wasn't started. Set up your authenticator app again."}
	case errors.Is(err, auth.ErrBadCredentials):
		return &apiError{401, "bad_credentials", "That password doesn't match."}
	case errors.Is(err, auth.ErrLocked):
		return locked()
	case errors.Is(err, auth.ErrBadCode):
		return &apiError{400, "invalid_code", "That code didn't work. Check your authenticator app and try the newest one."}
	case errors.Is(err, auth.ErrRateLimited):
		return tooManyAttempts()
	}
	return err
}

func (s *server) CreateRecoveryCodes(ctx context.Context, _ CreateRecoveryCodesRequestObject) (CreateRecoveryCodesResponseObject, error) {
	u, sess, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := s.Auth.GenerateRecoveryCodes(ctx, u.ID, sess.MFA)
	switch {
	case errors.Is(err, auth.ErrNoSecondFactor):
		return nil, &apiError{409, "no_second_factor", "Add a passkey or an authenticator app first. Recovery codes stand in for one of them."}
	case errors.Is(err, auth.ErrMFARequired):
		return nil, &apiError{403, "mfa_required", "Sign in with your passkey or authenticator code first, then create recovery codes."}
	case errors.Is(err, auth.ErrRateLimited):
		return nil, tooManyAttempts()
	case err != nil:
		return nil, err
	}
	return CreateRecoveryCodes200JSONResponse{Codes: codes}, nil
}

func (s *server) StartTotp(ctx context.Context, req StartTotpRequestObject) (StartTotpResponseObject, error) {
	u, sess, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	password := ""
	if req.Body != nil && req.Body.Password != nil {
		password = *req.Body.Password
	}
	secret, url, err := s.Auth.StartTOTP(ctx, u.ID, sess.MFA, password)
	if err != nil {
		return nil, totpError(err)
	}
	return StartTotp200JSONResponse{Secret: secret, OtpauthUrl: url}, nil
}

func (s *server) ConfirmTotp(ctx context.Context, req ConfirmTotpRequestObject) (ConfirmTotpResponseObject, error) {
	u, sess, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.Auth.ConfirmTOTP(ctx, u.ID, sess.MFA, req.Body.Code); err != nil {
		return nil, totpError(err)
	}
	return ConfirmTotp204Response{}, nil
}

// Access says who may call a tenant operation. Roles limits members (empty
// means any member); Write marks a write, which a read API key may not do;
// SessionOnly shuts API keys out entirely.
type Access struct {
	Roles       []string
	Write       bool
	SessionOnly bool
}

var errScopeReadOnly = &apiError{403, "scope_read_only", "This API key can only read. Create a key with write access to change things."}

// inOrganizer runs fn inside a transaction scoped to the organizer named by
// slug. Order matters: a non-member gets 404 before any role or MFA check, so
// the existence of an organizer never leaks. Owners and admins need an MFA
// session. An API key reaches only its own organizer, needs no MFA, and is
// refused SessionOnly operations before anything else, whatever the slug.
// Every /organizers/{organizer}/... handler must go through here.
func (s *server) inOrganizer(ctx context.Context, slug string, access Access, fn func(org organizers.Organizer, tx pgx.Tx) error) error {
	a, ok := ctx.Value(authKey).(authed)
	if !ok {
		return unauthorized()
	}
	if a.isKey() {
		return s.inOrganizerAsKey(ctx, a.key, slug, access, fn)
	}
	org, err := s.Organizers.Resolve(ctx, a.user.ID, slug)
	if errors.Is(err, organizers.ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return err
	}
	err = db.WithOrganizer(ctx, s.Pool, a.user.ID, org.ID, func(tx pgx.Tx, role string) error {
		if len(access.Roles) > 0 && !slices.Contains(access.Roles, role) {
			return &apiError{403, "forbidden_role", "Your role here doesn't allow that."}
		}
		if (role == "owner" || role == "admin") && !a.sess.MFA {
			return &apiError{403, "mfa_required", "Sign in with a passkey or your authenticator app to manage this organizer."}
		}
		org.Role = role
		return fn(org, tx)
	})
	if errors.Is(err, db.ErrNotMember) {
		return notFound()
	}
	return err
}

// inOrganizerAsKey resolves slug inside the key's own scope, where RLS shows
// only the key's organizer, so another tenant's slug is a 404 exactly as for
// a stranger.
func (s *server) inOrganizerAsKey(ctx context.Context, key db.Principal, slug string, access Access, fn func(org organizers.Organizer, tx pgx.Tx) error) error {
	if access.SessionOnly {
		return errSessionRequired
	}
	err := db.WithPrincipal(ctx, s.Pool, key, key.KeyOrg, func(tx pgx.Tx, role string) error {
		org := organizers.Organizer{Role: role}
		err := tx.QueryRow(ctx, `select id, name, slug::text from organizers where slug = $1`, slug).Scan(&org.ID, &org.Name, &org.Slug)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && org.ID != key.KeyOrg) {
			return notFound()
		}
		if err != nil {
			return err
		}
		if access.Write && key.KeyScope != "write" {
			return errScopeReadOnly
		}
		return fn(org, tx)
	})
	if errors.Is(err, db.ErrNotMember) {
		return notFound()
	}
	return err
}

var (
	slugRE         = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])*$`)
	errInvalidName = &apiError{422, "invalid_name", "Give your organizer a name of up to 120 characters."}
	errInvalidSlug = &apiError{422, "invalid_slug", "Use 3 to 40 lowercase letters, numbers and single hyphens."}
)

func toOrganizer(o organizers.Organizer) Organizer {
	return Organizer{Id: o.ID, Name: o.Name, Slug: o.Slug, Role: OrganizerRole(o.Role)}
}

func (s *server) ListOrganizers(ctx context.Context, _ ListOrganizersRequestObject) (ListOrganizersResponseObject, error) {
	u, _, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.Organizers.ListForUser(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	out := make(ListOrganizers200JSONResponse, 0, len(list))
	for _, o := range list {
		out = append(out, toOrganizer(o))
	}
	return out, nil
}

func (s *server) CreateOrganizer(ctx context.Context, req CreateOrganizerRequestObject) (CreateOrganizerResponseObject, error) {
	u, _, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	if !u.EmailVerified {
		return nil, &apiError{403, "email_unverified", "Check your inbox and verify your email before creating an organizer."}
	}
	name := strings.TrimSpace(req.Body.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 120 {
		return nil, errInvalidName
	}
	if !slugRE.MatchString(req.Body.Slug) || len(req.Body.Slug) < 3 || len(req.Body.Slug) > 40 {
		return nil, errInvalidSlug
	}
	o, err := s.Organizers.Create(ctx, u.ID, name, req.Body.Slug)
	if errors.Is(err, organizers.ErrInvalid) {
		return nil, errInvalidSlug
	}
	if errors.Is(err, organizers.ErrSlugTaken) {
		return nil, &apiError{409, "slug_taken", "Someone already uses that web address. Try another."}
	}
	if err != nil {
		return nil, err
	}
	return CreateOrganizer201JSONResponse(toOrganizer(o)), nil
}

func (s *server) GetOrganizer(ctx context.Context, req GetOrganizerRequestObject) (GetOrganizerResponseObject, error) {
	var out Organizer
	err := s.inOrganizer(ctx, req.Organizer, Access{}, func(org organizers.Organizer, _ pgx.Tx) error {
		out = toOrganizer(org)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return GetOrganizer200JSONResponse(out), nil
}

func (s *server) ListAudit(ctx context.Context, req ListAuditRequestObject) (ListAuditResponseObject, error) {
	out := ListAudit200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, SessionOnly: true}, func(_ organizers.Organizer, tx pgx.Tx) error {
		// Names come from the member directory (current members only) and
		// api_keys (revoked keys included); a former member's name is null.
		rows, err := tx.Query(ctx, `select a.id, a.action, a.entity_type, a.entity_id, a.actor_user_id, a.actor_api_key_id, a.at,
				coalesce(d.name, k.name), d.email
			from audit_log a
			left join app_member_directory() d on d.user_id = a.actor_user_id
			left join api_keys k on k.id = a.actor_api_key_id
			order by a.at desc, a.id desc limit 100`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e AuditEntry
			var name, email *string
			if err := rows.Scan(&e.Id, &e.Action, &e.EntityType, &e.EntityId, &e.ActorUserId, &e.ActorApiKeyId, &e.At, &name, &email); err != nil {
				return err
			}
			e.ActorName, e.ActorEmail = nullOf(name), nullOf(email)
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var ownerAdmin = []string{"owner", "admin"}

func toAPIKey(k apikeys.Key) ApiKey {
	return ApiKey{Id: k.ID, Name: k.Name, Prefix: k.Prefix, Scope: ApiKeyScope(k.Scope), CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt}
}

// Key management is SessionOnly: keys never mint, list or revoke keys.
func (s *server) ListApiKeys(ctx context.Context, req ListApiKeysRequestObject) (ListApiKeysResponseObject, error) {
	out := ListApiKeys200JSONResponse{}
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, SessionOnly: true}, func(_ organizers.Organizer, tx pgx.Tx) error {
		keys, err := s.Keys.List(ctx, tx)
		for _, k := range keys {
			out = append(out, toAPIKey(k))
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *server) CreateApiKey(ctx context.Context, req CreateApiKeyRequestObject) (CreateApiKeyResponseObject, error) {
	var out CreateApiKey201JSONResponse
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, Write: true, SessionOnly: true}, func(org organizers.Organizer, tx pgx.Tx) error {
		u, _, err := userFrom(ctx)
		if err != nil {
			return err
		}
		k, plain, err := s.Keys.Create(ctx, tx, org.ID, u.ID, req.Body.Name, string(req.Body.Scope))
		switch {
		case errors.Is(err, apikeys.ErrInvalidName):
			return &apiError{422, "invalid_name", "Give your key a name of up to 80 characters, so you know later what uses it."}
		case errors.Is(err, apikeys.ErrInvalidScope):
			return &apiError{422, "invalid_scope", "Pick read or write for the key's access."}
		case err != nil:
			return err
		}
		out = CreateApiKey201JSONResponse{ApiKey: toAPIKey(k), Token: plain}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *server) RevokeApiKey(ctx context.Context, req RevokeApiKeyRequestObject) (RevokeApiKeyResponseObject, error) {
	err := s.inOrganizer(ctx, req.Organizer, Access{Roles: ownerAdmin, Write: true, SessionOnly: true}, func(org organizers.Organizer, tx pgx.Tx) error {
		u, _, err := userFrom(ctx)
		if err != nil {
			return err
		}
		err = s.Keys.Revoke(ctx, tx, org.ID, u.ID, req.ApiKey)
		if errors.Is(err, apikeys.ErrNotFound) {
			return notFound()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return RevokeApiKey204Response{}, nil
}
