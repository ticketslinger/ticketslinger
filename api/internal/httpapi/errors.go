package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Code }

// fieldError is an apiError about one request property: the body names it as
// "field" (price_cents, options[2]).
type fieldError struct {
	*apiError
	Field string
}

func (e fieldError) Unwrap() error { return e.apiError }

// invalidField is a 422 about the request property field.
func invalidField(code, field, msg string) error {
	return fieldError{&apiError{422, code, msg}, field}
}

func unauthorized() error {
	return &apiError{401, "unauthorized", "Please sign in to continue."}
}
func tooManyAttempts() error {
	return &apiError{429, "rate_limited", "Too many attempts. Try again in a few minutes."}
}
func locked() error {
	return &apiError{423, "locked", "Too many wrong passwords. Your account is paused for 15 minutes."}
}

// The payment provider's errors. Both are 5xx, so Idempotency-Key never
// keeps them and a retry runs fresh.
var (
	errPaymentsNotConfigured = &apiError{503, "payments_not_configured", "This server isn't set up to take card payments yet. Free tickets still work. Paid ones need Stripe keys from whoever runs it."}
	errPaymentsUnavailable   = &apiError{503, "payments_unavailable", "We couldn't reach Stripe just now. Try again in a minute."}
	errNotConnected          = &apiError{409, "not_connected", "There's no Stripe account to check yet. Connect one first."}
)

func notFound() error {
	return &apiError{404, "not_found", "That doesn't exist, or you don't have access to it."}
}

// writeBadRequest answers a request that couldn't be decoded. The decoder's
// detail goes to the log, not the caller.
func writeBadRequest(w http.ResponseWriter, r *http.Request, err error) {
	slog.InfoContext(r.Context(), "bad request", "path", r.URL.Path, "err", err)
	writeError(w, &apiError{400, "bad_request", "That request couldn't be read."})
}

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.Is(err, sales.ErrBusy) {
		// Not stored by Idempotency-Key (5xx), so the buyer's retry runs fresh.
		w.Header().Set("Retry-After", "1")
		err = &apiError{503, "try_again", "Tickets are moving fast. Give it a second and try again."}
	}
	if !errors.As(err, &ae) {
		slog.Error("unhandled error", "err", err)
		ae = &apiError{500, "internal", "Something went wrong on our side. Please try again."}
	}
	body := map[string]string{"code": ae.Code, "message": ae.Message}
	var fe fieldError
	if errors.As(err, &fe) {
		body["field"] = fe.Field
	}
	writeJSON(w, ae.Status, map[string]any{"error": body})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic", "value", v, "path", r.URL.Path)
				writeError(w, errors.New("panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
