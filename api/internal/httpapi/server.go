package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

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

type server struct{ Deps }

func New(d Deps) http.Handler {
	s := &server{d}
	strict := NewStrictHandlerWithOptions(s, []StrictMiddlewareFunc{s.authenticate}, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  func(w http.ResponseWriter, _ *http.Request, err error) { writeError(w, badRequest(err.Error())) },
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) { writeError(w, err) },
	})
	mux := http.NewServeMux()
	HandlerWithOptions(strict, StdHTTPServerOptions{
		BaseURL:    "/v1",
		BaseRouter: mux,
		// Path-parameter parse failures from the generated wrapper.
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) { writeError(w, badRequest(err.Error())) },
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeError(w, notFound()) })
	return requestID(recoverer(mux))
}

// authenticate is a pass-through until sessions land.
func (s *server) authenticate(next StrictHandlerFunc, _ string) StrictHandlerFunc { return next }

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

// Operations not built yet.

func (s *server) Login(context.Context, LoginRequestObject) (LoginResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) Logout(context.Context, LogoutRequestObject) (LogoutResponseObject, error) {
	return nil, errNotImplemented
}

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

func (s *server) Signup(context.Context, SignupRequestObject) (SignupResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) StartTotp(context.Context, StartTotpRequestObject) (StartTotpResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) ConfirmTotp(context.Context, ConfirmTotpRequestObject) (ConfirmTotpResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) VerifyEmail(context.Context, VerifyEmailRequestObject) (VerifyEmailResponseObject, error) {
	return nil, errNotImplemented
}

func (s *server) GetMe(context.Context, GetMeRequestObject) (GetMeResponseObject, error) {
	return nil, errNotImplemented
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
