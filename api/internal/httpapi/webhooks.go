package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"time"

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
	ev, err := webhook.ConstructEventWithOptions(payload, r.Header.Get("Stripe-Signature"), s.Config.StripeWebhookSecret,
		webhook.ConstructEventOptions{Tolerance: 5 * time.Minute})
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
