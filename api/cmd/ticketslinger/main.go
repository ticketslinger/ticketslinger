// Command ticketslinger runs the TicketSlinger API and its maintenance tasks.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
	"github.com/ticketslinger/ticketslinger/api/internal/turnstile"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "migrate":
		cfg, err := config.Load()
		if err == nil && cfg.MigrateDatabaseURL == "" {
			err = errors.New("TS_MIGRATE_DATABASE_URL is required for migrate")
		}
		if err == nil {
			err = db.Migrate(context.Background(), cfg.MigrateDatabaseURL)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ticketslinger <serve|migrate|version>")
	os.Exit(2)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.SecretKey == nil {
		return errors.New("TS_SECRET_KEY is required (base64 of 32 random bytes, e.g. `openssl rand -base64 32`)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	handler, worker, err := newHandler(cfg, pool, os.Getenv("TS_TEST_MODE") == "1")
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	workerDone := make(chan struct{})
	go func() { worker.Run(ctx); close(workerDone) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	select {
	case <-workerDone:
	case <-shutdownCtx.Done():
	}
	return err
}

// newWorker is the job worker with every handler registered. It sends mail
// through m and refunds through pay, the same ones the API uses.
func newWorker(pool *pgxpool.Pool, m mailer.Mailer, cfg config.Config, pay plugin.PaymentProvider) *jobs.Worker {
	// Order emails only read orders and rebuild links; refunds need the provider.
	shop := &sales.Service{Pool: pool, StorefrontURL: cfg.StorefrontURL, SecretKey: cfg.SecretKey, Payments: pay}
	return &jobs.Worker{Pool: pool, Handlers: map[string]jobs.Handler{
		"send_email":               jobs.SendEmail(m),
		sales.OrderConfirmationJob: shop.OrderConfirmation(m),
		sales.RefundJob:            sales.Refund(shop),
	}, Parked: map[string]func(context.Context, []byte, string){sales.RefundJob: sales.RefundParked(shop)}}
}

// newHandler builds the API and its job worker, which share one mailer and
// one payment provider. In test mode both send mail to memory, and the API
// adds GET /__test/last-email and POST /__test/stripe/pay-latest for the
// end-to-end suite, and only on a loopback address.
func newHandler(cfg config.Config, pool *pgxpool.Pool, testMode bool) (http.Handler, *jobs.Worker, error) {
	var mail mailer.Mailer = mailer.Log{}
	mem := &mailer.Memory{}
	if testMode {
		// A same-host reverse proxy also reaches the API on loopback, so the
		// public URLs have to be local too; production's https URLs fail here.
		if host, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil || !isLocal(host) {
			return nil, nil, fmt.Errorf("TS_TEST_MODE needs a loopback TS_HTTP_ADDR, not %q", cfg.HTTPAddr)
		}
		for name, v := range map[string]string{"TS_PUBLIC_URL": cfg.PublicURL, "TS_DASHBOARD_ORIGIN": cfg.DashboardOrigin} {
			if u, err := url.Parse(v); err != nil || u.Scheme != "http" || !isLocal(u.Hostname()) {
				return nil, nil, fmt.Errorf("TS_TEST_MODE needs an http://localhost or loopback %s, not %q", name, v)
			}
		}
		// Test mode never touches real money, even by mistake.
		for name, key := range map[string]string{"TS_STRIPE_SECRET_KEY": cfg.StripeSecretKey, "TS_STRIPE_PUBLISHABLE_KEY": cfg.StripePublishableKey} {
			if mode, _ := config.StripeMode(key, "sk_", "rk_", "pk_"); mode == "live" {
				return nil, nil, fmt.Errorf("TS_TEST_MODE refuses a live %s; use a test key or none", name)
			}
		}
		mail = mem
	} else if cfg.TurnstileSecret == "" {
		// Sign-in's account-wide brake leans on Turnstile; without it, the
		// account throttle is all that stands between a botnet and a password.
		if os.Getenv("TS_ALLOW_NO_TURNSTILE") != "1" {
			return nil, nil, errors.New("TS_TURNSTILE_SECRET is required: it guards sign-up, repeated failed sign-ins and checkouts. " +
				"Set TS_ALLOW_NO_TURNSTILE=1 to run without it anyway")
		}
		slog.Warn("RUNNING WITHOUT TURNSTILE (TS_ALLOW_NO_TURNSTILE=1): sign-up and checkouts go unchecked, " +
			"and an account under attack is only slowed to one sign-in attempt every 10 seconds")
	}
	pay := paymentProvider(cfg, testMode)
	ts := &turnstile.Verifier{Secret: cfg.TurnstileSecret, URL: cfg.TurnstileVerifyURL, Client: &http.Client{Timeout: 2 * time.Second}}
	h := httpapi.New(httpapi.Deps{Pool: pool, Auth: auth.NewService(pool, mail, auth.Options{RPID: cfg.WebAuthnRPID, Origin: cfg.DashboardOrigin, SecretKey: cfg.SecretKey,
		Turnstile: ts}),
		Config: cfg, Payments: pay, Turnstile: ts})
	if testMode {
		fake, _ := pay.(*payments.Fake)
		h = testRoutes(h, mem, fake, cfg.StripeWebhookSecret)
	}
	return h, newWorker(pool, mail, cfg, pay), nil
}

// paymentProvider is the fake in test mode, Stripe when a secret key is set,
// and otherwise none: free tickets still sell, paid ones can't.
func paymentProvider(cfg config.Config, testMode bool) plugin.PaymentProvider {
	switch {
	case testMode:
		return payments.NewFake()
	case cfg.StripeSecretKey != "":
		return payments.NewStripe(cfg.StripeSecretKey, cfg.StripePublishableKey, cfg.StripeClientID, nil)
	}
	return nil
}

// testRoutes adds the end-to-end suite's helpers to api. Anything that came
// through a proxy gets the same 404 as when they aren't mounted.
//
// GET /__test/last-email serves the link in the newest message to ?to=, so
// a browser test can follow an email.
//
// POST /__test/stripe/pay-latest is what Stripe would send when the fake's
// newest payment succeeds, signed with our own webhook secret and served
// through the real webhook route.
//
// POST /__test/stripe/refund-latest?status=succeeded|failed is the
// refund.updated Stripe would send for the fake's newest refund, served the
// same way.
func testRoutes(api http.Handler, m *mailer.Memory, fake *payments.Fake, webhookSecret string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.HandleFunc("GET /__test/last-email", func(w http.ResponseWriter, r *http.Request) {
		link := m.LastLink(r.URL.Query().Get("to"))
		if proxied(r) || link == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, link)
	})
	mux.HandleFunc("POST /__test/stripe/pay-latest", func(w http.ResponseWriter, r *http.Request) {
		var pi plugin.PaymentIntent
		ok := fake != nil
		if ok {
			pi, ok = fake.Latest()
		}
		if proxied(r) || !ok {
			http.NotFound(w, r)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"id": "evt_test_" + pi.ID, "object": "event", "type": "payment_intent.succeeded", "api_version": stripe.APIVersion,
			"account": pi.Account, "created": time.Now().Unix(),
			"data": map[string]any{"object": map[string]any{"id": pi.ID, "object": "payment_intent", "amount": pi.AmountCents,
				"currency": "usd", "status": "succeeded"}},
		})
		serveSigned(w, r, api, body, webhookSecret)
	})
	mux.HandleFunc("POST /__test/stripe/refund-latest", func(w http.ResponseWriter, r *http.Request) {
		var in plugin.RefundInput
		var id string
		ok := fake != nil
		if ok {
			in, id, ok = fake.LatestRefund()
		}
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "succeeded"
		}
		if proxied(r) || !ok || status != "succeeded" && status != "failed" {
			http.NotFound(w, r)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"id": "evt_test_" + id + "_" + status, "object": "event", "type": "refund.updated", "api_version": stripe.APIVersion,
			"account": in.Account, "created": time.Now().Unix(),
			"data": map[string]any{"object": map[string]any{"id": id, "object": "refund", "amount": in.AmountCents,
				"payment_intent": in.PaymentIntentID, "status": status, "metadata": in.Metadata}},
		})
		serveSigned(w, r, api, body, webhookSecret)
	})
	return mux
}

// serveSigned serves body through api's webhook route, signed as Stripe would.
func serveSigned(w http.ResponseWriter, r *http.Request, api http.Handler, body []byte, secret string) {
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret})
	req := httptest.NewRequestWithContext(r.Context(), http.MethodPost, "/v1/webhooks/stripe", bytes.NewReader(signed.Payload))
	req.Header.Set("Stripe-Signature", signed.Header)
	api.ServeHTTP(w, req)
}

// proxied reports whether r came through a reverse proxy.
func proxied(r *http.Request) bool {
	for _, h := range []string{"X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// isLocal reports whether host is "localhost" or a loopback IP.
func isLocal(host string) bool {
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || err == nil && ip.IsLoopback()
}
