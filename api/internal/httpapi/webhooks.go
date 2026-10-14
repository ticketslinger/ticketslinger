package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"
)

// stripeWebhook verifies Stripe's signature over the raw body and hands the
// event to sales. A 5xx makes Stripe retry; stripe_events makes retries
// harmless. It sits on the mux outside the spec and the Idempotency-Key
// middleware: it needs the body exactly as sent. With no webhook secret
// (Stripe isn't configured) it's a 404 like any unknown route.
func (s *server) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Config.StripeWebhookSecret == "" {
		writeError(w, notFound())
		return
	}
	payload, err := io.ReadAll(r.Body) // limitBody caps it at 1 MiB
	if err != nil {
		writeError(w, &apiError{400, "bad_request", "That request couldn't be read."})
		return
	}
	sig := r.Header.Get("Stripe-Signature")
	// The signature first, before anything in the body is believed.
	if err := webhook.ValidatePayloadWithTolerance(payload, sig, s.Config.StripeWebhookSecret, webhookTolerance); err != nil {
		slog.WarnContext(r.Context(), "stripe webhook refused", "err", err)
		writeError(w, &apiError{400, "bad_signature", "Signature check failed."})
		return
	}
	// Every event must be in the API version stripe-go reads, except
	// account.application.deauthorized: Stripe sends those in the account's
	// default version whatever the endpoint asks for, and we read only its
	// account. ConstructEventWithOptions checks the signature again.
	var peek struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &peek)
	lenient := peek.Type == string(stripe.EventTypeAccountApplicationDeauthorized)
	ev, err := webhook.ConstructEventWithOptions(payload, sig, s.Config.StripeWebhookSecret, webhook.ConstructEventOptions{
		Tolerance: webhookTolerance, IgnoreAPIVersionMismatch: lenient})
	if err == nil && lenient && ev.Type != stripe.EventTypeAccountApplicationDeauthorized {
		err = errors.New("event type changed between the peek and the parse") // a leniently parsed event must be the one type allowed
	}
	if err != nil {
		slog.WarnContext(r.Context(), "stripe webhook refused", "err", err)
		writeError(w, &apiError{400, "bad_signature", "Signature check failed."})
		return
	}
	if err := s.Sales.HandleStripeEvent(r.Context(), ev); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"received": true})
}

const webhookTolerance = 5 * time.Minute

// privateAnswers keeps a buyer's order page out of caches and referrers: it
// carries the tickets' QR values.
func privateAnswers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Pattern == "GET /v1/orders/{code}" {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
		}
		next.ServeHTTP(w, r)
	})
}
