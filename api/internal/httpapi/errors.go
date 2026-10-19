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

// detailError is an apiError with more to say: extra goes inside "error"
// next to code and message (available on sold_out, order_code on
// checkout_completed).
type detailError struct {
	*apiError
	extra map[string]any
}

func (e detailError) Unwrap() error { return e.apiError }

// retryError is an apiError that says when to come back (Retry-After, in seconds).
type retryError struct {
	*apiError
	after string
}

func (e retryError) Unwrap() error { return e.apiError }

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

// The payment provider's errors. All are 5xx, so Idempotency-Key never
// keeps them and a retry runs fresh. Unavailable might work in a minute;
// refused won't, until someone fixes what Stripe objected to.
var (
	errPaymentsNotConfigured = &apiError{503, "payments_not_configured", "This server isn't set up to take card payments yet. Free tickets still work. Paid ones need Stripe keys from whoever runs it."}
	errPaymentsUnavailable   = &apiError{503, "payments_unavailable", "We couldn't reach Stripe just now. Try again in a minute."}
	errPaymentsRefused       = &apiError{503, "payments_refused", "Stripe turned this down. We've logged the details and we'll look into it."}
	errOAuthNotConfigured    = &apiError{503, "oauth_not_configured", "This server can't link an existing Stripe account yet. You can still create a new one here."}
	errNotConnected          = &apiError{409, "not_connected", "There's no Stripe account to check yet. Connect one first."}
	errNothingToDisconnect   = &apiError{409, "not_connected", "There's no Stripe account connected, so there's nothing to disconnect."}
	errAlreadyConnected      = &apiError{409, "already_connected", "This organizer already has a Stripe account connected. Disconnect it first, then link the other one."}
	errAccountInUse          = &apiError{409, "account_in_use", "That Stripe account is already taking payments for another organizer here. Disconnect it there first, or use a different account."}
	errPaymentsInProgress    = &apiError{409, "payments_in_progress", "Someone's paying for tickets right now. Give it a few minutes, then disconnect."}
	errDirectPayments        = &apiError{409, "direct_payments", "This server takes payments through its own Stripe account, so there's nothing to connect here."}
	errInvalidState          = &apiError{400, "invalid_state", "That link has expired. Start again from your organizer's Payments page."}
)

// errTurnstileUnavailable: Cloudflare couldn't check a Turnstile token. A 5xx,
// so Idempotency-Key never keeps it.
var errTurnstileUnavailable = &apiError{503, "turnstile_unavailable", "We couldn't run the quick person check just now. Try again in a minute."}

func notFound() error {
	return &apiError{404, "not_found", "That doesn't exist, or you don't have access to it."}
}

// route names r's endpoint for a log line by its mux pattern
// ("POST /v1/checkouts/{token}/pay"), never its path: a path can carry a
// bearer token (a checkout's), and logs are read far more widely.
func route(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	return r.Pattern
}

// writeBadRequest answers a request that couldn't be decoded. The decoder's
// detail goes to the log, not the caller.
func writeBadRequest(w http.ResponseWriter, r *http.Request, err error) {
	slog.InfoContext(r.Context(), "bad request", "route", route(r), "err", err)
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
	var re retryError
	if errors.As(err, &re) {
		w.Header().Set("Retry-After", re.after)
	}
	body := map[string]any{"code": ae.Code, "message": ae.Message}
	var fe fieldError
	if errors.As(err, &fe) {
		body["field"] = fe.Field
	}
	var de detailError
	if errors.As(err, &de) {
		for k, v := range de.extra {
			body[k] = v
		}
	}
	writeJSON(w, ae.Status, map[string]any{"error": body})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				// The mux set r.Pattern on this same request before the handler ran.
				slog.Error("panic", "value", v, "route", route(r))
				writeError(w, errors.New("panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
