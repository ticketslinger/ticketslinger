// Command ticketslinger runs the TicketSlinger API and its maintenance tasks.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
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
	if cfg.TurnstileSecret == "" {
		slog.Warn("TS_TURNSTILE_SECRET is unset: sign-up, repeated failed sign-ins and checkouts go without a Turnstile check")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	handler, err := newHandler(cfg, pool, os.Getenv("TS_TEST_MODE") == "1")
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
	go func() { newWorker(pool, mailer.Log{}).Run(ctx); close(workerDone) }()
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

// newWorker is the job worker with every handler registered.
func newWorker(pool *pgxpool.Pool, m mailer.Mailer) *jobs.Worker {
	return &jobs.Worker{Pool: pool, Handlers: map[string]jobs.Handler{"send_email": jobs.SendEmail(m)}}
}

// newHandler builds the API. In test mode it sends mail to memory and adds
// GET /__test/last-email for the end-to-end suite, and only on a loopback address.
func newHandler(cfg config.Config, pool *pgxpool.Pool, testMode bool) (http.Handler, error) {
	var mail mailer.Mailer = mailer.Log{}
	mem := &mailer.Memory{}
	if testMode {
		// A same-host reverse proxy also reaches the API on loopback, so the
		// public URLs have to be local too; production's https URLs fail here.
		if host, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil || !isLocal(host) {
			return nil, fmt.Errorf("TS_TEST_MODE needs a loopback TS_HTTP_ADDR, not %q", cfg.HTTPAddr)
		}
		for name, v := range map[string]string{"TS_PUBLIC_URL": cfg.PublicURL, "TS_DASHBOARD_ORIGIN": cfg.DashboardOrigin} {
			if u, err := url.Parse(v); err != nil || u.Scheme != "http" || !isLocal(u.Hostname()) {
				return nil, fmt.Errorf("TS_TEST_MODE needs an http://localhost or loopback %s, not %q", name, v)
			}
		}
		// Test mode never touches real money, even by mistake.
		for name, key := range map[string]string{"TS_STRIPE_SECRET_KEY": cfg.StripeSecretKey, "TS_STRIPE_PUBLISHABLE_KEY": cfg.StripePublishableKey} {
			if mode, _ := config.StripeMode(key, "sk_", "rk_", "pk_"); mode == "live" {
				return nil, fmt.Errorf("TS_TEST_MODE refuses a live %s; use a test key or none", name)
			}
		}
		mail = mem
	}
	ts := &turnstile.Verifier{Secret: cfg.TurnstileSecret, URL: cfg.TurnstileVerifyURL, Client: &http.Client{Timeout: 2 * time.Second}}
	h := httpapi.New(httpapi.Deps{Pool: pool, Auth: auth.NewService(pool, mail, auth.Options{RPID: cfg.WebAuthnRPID, Origin: cfg.DashboardOrigin, SecretKey: cfg.SecretKey,
		Turnstile: ts}),
		Config: cfg, Payments: paymentProvider(cfg, testMode), Turnstile: ts})
	if testMode {
		h = testMail(h, mem)
	}
	return h, nil
}

// paymentProvider is the fake in test mode, Stripe when a secret key is set,
// and otherwise none: free tickets still sell, paid ones can't.
func paymentProvider(cfg config.Config, testMode bool) plugin.PaymentProvider {
	switch {
	case testMode:
		return payments.NewFake()
	case cfg.StripeSecretKey != "":
		return payments.NewStripe(cfg.StripeSecretKey, cfg.StripePublishableKey, nil)
	}
	return nil
}

// testMail serves the link in the newest message to ?to=, so a browser test
// can follow the verification email. Anything that came through a proxy gets
// the same 404 as when the route isn't mounted.
func testMail(api http.Handler, m *mailer.Memory) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.HandleFunc("GET /__test/last-email", func(w http.ResponseWriter, r *http.Request) {
		proxied := false
		for _, h := range []string{"X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
			proxied = proxied || r.Header.Get(h) != ""
		}
		link := m.LastLink(r.URL.Query().Get("to"))
		if proxied || link == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, link)
	})
	return mux
}

// isLocal reports whether host is "localhost" or a loopback IP.
func isLocal(host string) bool {
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || err == nil && ip.IsLoopback()
}
