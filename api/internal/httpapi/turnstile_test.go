package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
	"github.com/ticketslinger/ticketslinger/api/internal/config"
	"github.com/ticketslinger/ticketslinger/api/internal/httpapi"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
	"github.com/ticketslinger/ticketslinger/api/internal/turnstile"
)

const turnstileSecret = "0x4AAAAAAA-very-secret"

// guardedServer runs the API behind a trusted local proxy with Turnstile
// checked by siteverify (nil: Turnstile off).
func guardedServer(t *testing.T, siteverify http.HandlerFunc) *httptest.Server {
	var v *turnstile.Verifier
	if siteverify != nil {
		cf := httptest.NewServer(siteverify) // raw: stands in for Cloudflare, not the API
		t.Cleanup(cf.Close)
		v = &turnstile.Verifier{Secret: turnstileSecret, URL: cf.URL, Client: cf.Client()}
	}
	pool := testdb.New(t)
	svc := auth.NewService(pool, &mailer.Memory{}, auth.Options{Origin: "http://localhost:5173", SecretKey: []byte("0123456789abcdef0123456789abcdef"), Turnstile: v})
	cfg := config.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}}
	return startServer(t, httpapi.New(httpapi.Deps{Pool: pool, Auth: svc, Config: cfg}))
}

// acceptOK passes the token "ok" and nothing else.
func acceptOK(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]bool{"success": r.FormValue("response") == "ok"})
}

func errField(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	f, _ := e["field"].(string)
	return f
}

func TestSignupNeedsTurnstileWhenConfigured(t *testing.T) {
	srv := guardedServer(t, acceptOK)
	for _, body := range []string{
		`{"email":"a@example.com","password":"a long passphrase","name":"A"}`,
		`{"email":"a@example.com","password":"a long passphrase","name":"A","turnstile_token":"forged"}`,
	} {
		if code, b := do(t, "POST", srv.URL+"/v1/auth/signup", "", body); code != 422 || errCode(b) != "turnstile_failed" || errField(b) != "turnstile_token" {
			t.Fatalf("%s: %d %v", body, code, b)
		}
	}
	if code, b := do(t, "POST", srv.URL+"/v1/auth/signup", "", `{"email":"a@example.com","password":"a long passphrase","name":"A","turnstile_token":"ok"}`); code != 201 {
		t.Fatalf("with a token: %d %v", code, b)
	}
}

func TestLoginLockoutIsPerAddressOverHTTP(t *testing.T) {
	srv := guardedServer(t, nil)
	do(t, "POST", srv.URL+"/v1/auth/signup", "", `{"email":"a@example.com","password":"a long passphrase","name":"A"}`)
	wrong := `{"email":"a@example.com","password":"wrong password"}`
	for i := 0; i < 5; i++ {
		if code, b := doFrom(t, "203.0.113.1", "POST", srv.URL+"/v1/auth/login", "", wrong); code != 401 || errCode(b) != "bad_credentials" {
			t.Fatalf("failure %d: %d %v", i, code, b)
		}
	}
	right := `{"email":"a@example.com","password":"a long passphrase"}`
	if code, b := doFrom(t, "203.0.113.1", "POST", srv.URL+"/v1/auth/login", "", right); code != 423 || errCode(b) != "locked" {
		t.Fatalf("same address: %d %v", code, b)
	}
	if code, b := doFrom(t, "198.51.100.7", "POST", srv.URL+"/v1/auth/login", "", right); code != 200 {
		t.Fatalf("another address: %d %v", code, b)
	}
}

func TestLoginAsksForTurnstileAfterThreeFailures(t *testing.T) {
	srv := guardedServer(t, acceptOK)
	do(t, "POST", srv.URL+"/v1/auth/signup", "", `{"email":"a@example.com","password":"a long passphrase","name":"A","turnstile_token":"ok"}`)
	for i := 0; i < 3; i++ {
		doFrom(t, "203.0.113.1", "POST", srv.URL+"/v1/auth/login", "", `{"email":"a@example.com","password":"wrong password"}`)
	}
	code, b := doFrom(t, "203.0.113.1", "POST", srv.URL+"/v1/auth/login", "", `{"email":"a@example.com","password":"a long passphrase"}`)
	if code != 401 || errCode(b) != "turnstile_required" || errField(b) != "turnstile_token" {
		t.Fatalf("without a token: %d %v", code, b)
	}
	code, b = doFrom(t, "203.0.113.1", "POST", srv.URL+"/v1/auth/login", "", `{"email":"a@example.com","password":"a long passphrase","turnstile_token":"forged"}`)
	if code != 422 || errCode(b) != "turnstile_failed" || errField(b) != "turnstile_token" {
		t.Fatalf("refused token: %d %v", code, b)
	}
	if code, b := doFrom(t, "203.0.113.1", "POST", srv.URL+"/v1/auth/login", "", `{"email":"a@example.com","password":"a long passphrase","turnstile_token":"ok"}`); code != 200 {
		t.Fatalf("with a token: %d %v", code, b)
	}
}

// A siteverify outage fails closed with 503, and the secret shows up in
// neither the response nor the log.
func TestTurnstileOutageIs503(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := guardedServer(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", 500) })
	code, b := do(t, "POST", srv.URL+"/v1/auth/signup", "", `{"email":"a@example.com","password":"a long passphrase","name":"A","turnstile_token":"tok"}`)
	if code != 503 || errCode(b) != "turnstile_unavailable" {
		t.Fatalf("got %d %v", code, b)
	}
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), turnstileSecret) || strings.Contains(logs.String(), turnstileSecret) {
		t.Fatalf("secret leaked: %s / %s", raw, logs.String())
	}
	if !strings.Contains(logs.String(), "turnstile") {
		t.Fatalf("outage not logged: %q", logs.String())
	}
}
