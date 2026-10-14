package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

func TestVersionIsSet(t *testing.T) {
	if version == "" {
		t.Fatal("version must default to a non-empty string")
	}
}

// local is a test-mode config that serve accepts.
func local(addr string) config.Config {
	return config.Config{HTTPAddr: addr, PublicURL: "http://127.0.0.1:3001", DashboardOrigin: "http://localhost:3002"}
}

func get(t *testing.T, h http.Handler, path string, header ...string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestTestMailEndpointIsAbsentWithoutTestMode(t *testing.T) {
	h, err := newHandler(config.Config{HTTPAddr: "127.0.0.1:3000"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := get(t, h, "/__test/last-email?to=a@example.com"); code != 404 || !strings.Contains(body, "not_found") {
		t.Fatalf("got %d %s, want the API's own 404", code, body)
	}
}

func TestTestModeRefusesNonLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:3000", ":3000", "[::]:3000", "192.168.1.5:3000", "example.com:3000"} {
		if _, err := newHandler(local(addr), nil, true); err == nil {
			t.Errorf("%s: test mode started on a non-loopback address", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:3000", "[::1]:3000", "localhost:3000"} {
		if _, err := newHandler(local(addr), nil, true); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

// Behind a same-host reverse proxy the API listens on loopback too, so the
// public URLs must be local as well before test mode will start.
func TestTestModeRefusesPublicURLs(t *testing.T) {
	for name, mod := range map[string]func(*config.Config){
		"https public URL":           func(c *config.Config) { c.PublicURL = "https://api.ticketslinger.com" },
		"https loopback public URL":  func(c *config.Config) { c.PublicURL = "https://127.0.0.1:3001" },
		"public dashboard origin":    func(c *config.Config) { c.DashboardOrigin = "http://app.ticketslinger.com" },
		"LAN dashboard origin":       func(c *config.Config) { c.DashboardOrigin = "http://192.168.1.5:3002" },
		"lookalike dashboard origin": func(c *config.Config) { c.DashboardOrigin = "http://localhost.example.com" },
	} {
		c := local("127.0.0.1:3001")
		mod(&c)
		if _, err := newHandler(c, nil, true); err == nil {
			t.Errorf("%s: test mode started", name)
		}
	}
}

func TestTestMailRefusesProxiedRequests(t *testing.T) {
	m := &mailer.Memory{}
	_ = m.Send(context.Background(), "a@example.com", "Confirm", "Open http://localhost:3002/verify-email?token=t")
	h := testMail(http.NotFoundHandler(), m)
	for _, hdr := range []string{"X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if code, body := get(t, h, "/__test/last-email?to=a@example.com", hdr, "203.0.113.9"); code != 404 {
			t.Errorf("%s: got %d %q, want 404", hdr, code, body)
		}
	}
}

func TestTestMailReturnsTheNewestLinkForThatAddress(t *testing.T) {
	m := &mailer.Memory{}
	ctx := context.Background()
	_ = m.Send(ctx, "a@example.com", "Confirm", "Open http://localhost:3002/verify-email?token=old to confirm.")
	_ = m.Send(ctx, "a@example.com", "Confirm", "Open http://localhost:3002/verify-email?token=new to confirm.")
	_ = m.Send(ctx, "b@example.com", "Confirm", "Open http://localhost:3002/verify-email?token=bee to confirm.")
	h := testMail(http.NotFoundHandler(), m)

	if code, body := get(t, h, "/__test/last-email?to=a@example.com"); code != 200 || body != "http://localhost:3002/verify-email?token=new" {
		t.Fatalf("got %d %q", code, body)
	}
	if code, _ := get(t, h, "/__test/last-email?to=nobody@example.com"); code != 404 {
		t.Fatalf("no mail: got %d, want 404", code)
	}
}

func TestWorkerSendsQueuedEmail(t *testing.T) {
	m := &mailer.Memory{}
	h := newWorker(nil, m).Handlers["send_email"]
	if h == nil {
		t.Fatal("serve's worker has no send_email handler")
	}
	if err := h(context.Background(), []byte(`{"to":"a@example.com","subject":"Hi","text":"Hello"}`)); err != nil || len(m.Sent) != 1 {
		t.Fatalf("err=%v sent=%d", err, len(m.Sent))
	}
}

func TestTestModeRefusesLiveStripeKeys(t *testing.T) {
	for _, c := range []config.Config{
		{StripeSecretKey: "sk_live_abc", StripePublishableKey: "pk_live_abc", StripeWebhookSecret: "whsec_abc"},
		{StripeSecretKey: "rk_live_abc", StripePublishableKey: "pk_live_abc", StripeWebhookSecret: "whsec_abc"},
		{StripePublishableKey: "pk_live_abc"},
	} {
		cfg := local("127.0.0.1:3001")
		cfg.StripeSecretKey, cfg.StripePublishableKey, cfg.StripeWebhookSecret = c.StripeSecretKey, c.StripePublishableKey, c.StripeWebhookSecret
		if _, err := newHandler(cfg, nil, true); err == nil {
			t.Errorf("%+v: test mode started with a live key", c)
		} else if strings.Contains(err.Error(), "_abc") {
			t.Errorf("error %q repeats the key", err)
		}
		if _, err := newHandler(cfg, nil, false); err != nil {
			t.Errorf("%+v outside test mode: %v", c, err)
		}
	}
	cfg := local("127.0.0.1:3001")
	cfg.StripeSecretKey, cfg.StripePublishableKey, cfg.StripeWebhookSecret = "sk_test_abc", "pk_test_abc", "whsec_e2e"
	if _, err := newHandler(cfg, nil, true); err != nil {
		t.Fatalf("test keys in test mode: %v", err)
	}
}

func TestPaymentProvider(t *testing.T) {
	if p := paymentProvider(config.Config{}, false); p != nil {
		t.Fatalf("no key: %v, want none", p)
	}
	if p := paymentProvider(config.Config{StripeSecretKey: "sk_test_abc", StripePublishableKey: "pk_test_abc"}, false); p == nil || p.Name() != "stripe" || p.PublishableKey() != "pk_test_abc" {
		t.Fatalf("with a key: %v", p)
	}
	for _, c := range []config.Config{{}, {StripeSecretKey: "sk_test_abc", StripePublishableKey: "pk_test_abc"}} {
		if p := paymentProvider(c, true); p == nil || p.Name() != "test" {
			t.Fatalf("test mode: %v, want the fake", p)
		}
	}
}
