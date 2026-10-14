package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
)

type Deps struct {
	Pool       *pgxpool.Pool
	Auth       *auth.Service
	Organizers *organizers.Service
	Config     config.Config
}

type server struct {
	Deps
	public map[string]bool // Go operation names whose spec says security: []
}

func New(d Deps) http.Handler {
	s := &server{Deps: d, public: publicOperations()}
	strict := NewStrictHandlerWithOptions(s, []StrictMiddlewareFunc{s.authenticate}, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  func(w http.ResponseWriter, r *http.Request, err error) { writeBadRequest(w, r, err) },
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) { writeError(w, err) },
	})
	mux := http.NewServeMux()
	HandlerWithOptions(strict, StdHTTPServerOptions{
		BaseURL:    "/v1",
		BaseRouter: mux,
		// Path-parameter parse failures from the generated wrapper.
		ErrorHandlerFunc: writeBadRequest,
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeError(w, notFound()) })
	return requestID(recoverer(limitBody(mux)))
}

// publicOperations reads the embedded spec once and returns the operations
// that opt out of the global session requirement with an explicit `security: []`.
func publicOperations() map[string]bool {
	spec, err := GetSwagger()
	if err != nil {
		panic(err)
	}
	public := map[string]bool{}
	for _, item := range spec.Paths.Map() {
		for _, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				// oapi-codegen names operations by upper-casing the first letter of the operationId.
				public[strings.ToUpper(op.OperationID[:1])+op.OperationID[1:]] = true
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

type authed struct {
	user  auth.User
	sess  auth.Session
	token string
}

// authenticate records the caller's address and, for every operation that
// isn't public, requires a valid bearer session.
func (s *server) authenticate(next StrictHandlerFunc, op string) StrictHandlerFunc {
	public := s.public[op]
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		ctx = context.WithValue(ctx, clientKey, client{clientIP(r, s.Config.TrustedProxies), r.UserAgent()})
		if public {
			return next(ctx, w, r, req)
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			return nil, unauthorized()
		}
		u, sess, err := s.Auth.Authenticate(ctx, token)
		if errors.Is(err, auth.ErrBadToken) {
			return nil, unauthorized()
		}
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, authKey, authed{u, sess, token}), w, r, req)
	}
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

func userFrom(ctx context.Context) (auth.User, auth.Session, bool) {
	a, ok := ctx.Value(authKey).(authed)
	return a.user, a.sess, ok
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
	return Me{Id: u.ID, Email: u.Email, Name: u.Name, EmailVerified: u.EmailVerified, TotpEnabled: u.TOTPEnabled, Mfa: sess.MFA}
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
	sess, u, err := s.Auth.Login(ctx, string(req.Body.Email), req.Body.Password, code, c.ip, c.ua)
	switch {
	case errors.Is(err, auth.ErrBadCredentials):
		return nil, &apiError{401, "bad_credentials", "That email and password don't match."}
	case errors.Is(err, auth.ErrTOTPRequired):
		return nil, &apiError{401, "totp_required", "Enter the 6-digit code from your authenticator app."}
	case errors.Is(err, auth.ErrLocked):
		return nil, &apiError{423, "locked", "Too many wrong passwords. Your account is paused for 15 minutes."}
	case errors.Is(err, auth.ErrRateLimited):
		return nil, tooManyAttempts()
	case err != nil:
		return nil, err
	}
	return Login200JSONResponse{SessionToken: sess.Token, User: me(u, sess)}, nil
}

func (s *server) Logout(ctx context.Context, _ LogoutRequestObject) (LogoutResponseObject, error) {
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
	u, sess, ok := userFrom(ctx)
	if !ok {
		return nil, unauthorized()
	}
	return GetMe200JSONResponse(me(u, sess)), nil
}

// Operations not built yet.

func (s *server) BeginPasskeyLogin(context.Context, BeginPasskeyLoginRequestObject) (BeginPasskeyLoginResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) FinishPasskeyLogin(context.Context, FinishPasskeyLoginRequestObject) (FinishPasskeyLoginResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) BeginPasskeyRegistration(context.Context, BeginPasskeyRegistrationRequestObject) (BeginPasskeyRegistrationResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) FinishPasskeyRegistration(context.Context, FinishPasskeyRegistrationRequestObject) (FinishPasskeyRegistrationResponseObject, error) {
	return nil, errNotImplemented
}

func totpError(err error) error {
	switch {
	case errors.Is(err, auth.ErrTOTPAlreadyEnabled):
		return &apiError{409, "totp_already_enabled", "Two-factor sign-in is already switched on for your account."}
	case errors.Is(err, auth.ErrTOTPNotStarted):
		return &apiError{400, "totp_not_started", "Start setting up your authenticator app first."}
	case errors.Is(err, auth.ErrBadCode):
		return &apiError{400, "invalid_code", "That code didn't work. Check your authenticator app and try the newest one."}
	case errors.Is(err, auth.ErrRateLimited):
		return tooManyAttempts()
	}
	return err
}

func (s *server) StartTotp(ctx context.Context, _ StartTotpRequestObject) (StartTotpResponseObject, error) {
	u, _, ok := userFrom(ctx)
	if !ok {
		return nil, unauthorized()
	}
	secret, url, err := s.Auth.StartTOTP(ctx, u.ID)
	if err != nil {
		return nil, totpError(err)
	}
	return StartTotp200JSONResponse{Secret: secret, OtpauthUrl: url}, nil
}

func (s *server) ConfirmTotp(ctx context.Context, req ConfirmTotpRequestObject) (ConfirmTotpResponseObject, error) {
	u, _, ok := userFrom(ctx)
	if !ok {
		return nil, unauthorized()
	}
	if err := s.Auth.ConfirmTOTP(ctx, u.ID, req.Body.Code); err != nil {
		return nil, totpError(err)
	}
	return ConfirmTotp204Response{}, nil
}

func (s *server) ListOrganizers(context.Context, ListOrganizersRequestObject) (ListOrganizersResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) CreateOrganizer(context.Context, CreateOrganizerRequestObject) (CreateOrganizerResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) GetOrganizer(context.Context, GetOrganizerRequestObject) (GetOrganizerResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) ListAudit(context.Context, ListAuditRequestObject) (ListAuditResponseObject, error) {
	return nil, errNotImplemented
}
