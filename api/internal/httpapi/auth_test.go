package httpapi_test

import (
	"time"

	"context"
	"encoding/json"
	"github.com/pquerna/otp/totp"
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
)

func newAuthServer(t *testing.T) (*httptest.Server, *mailer.Memory) {
	pool := testdb.New(t)
	m := &mailer.Memory{}
	svc := auth.NewService(pool, m, auth.Options{Origin: "http://localhost:5173", SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Pool: pool, Auth: svc}))
	t.Cleanup(srv.Close)
	return srv, m
}

func do(t *testing.T, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	return doFrom(t, "", method, url, token, body)
}

// doFrom sends a request as if relayed by a local proxy for realIP.
func doFrom(t *testing.T, realIP, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if realIP != "" {
		req.Header.Set("X-Real-IP", realIP)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestSignupMeLogout(t *testing.T) {
	srv, m := newAuthServer(t)
	code, body := do(t, "POST", srv.URL+"/v1/auth/signup", "", `{"email":"Ann@Example.com","password":"a long passphrase","name":"Ann"}`)
	if code != 201 {
		t.Fatalf("signup: %d %v", code, body)
	}
	tok, _ := body["session_token"].(string)

	if code, body = do(t, "GET", srv.URL+"/v1/me", tok, ""); code != 200 || body["email"] != "ann@example.com" || body["email_verified"] != false {
		t.Fatalf("me: %d %v", code, body)
	}
	if code, body = do(t, "GET", srv.URL+"/v1/me", "", ""); code != 401 || errCode(body) != "unauthorized" {
		t.Fatalf("me without token: %d %v", code, body)
	}
	if code, _ = do(t, "GET", srv.URL+"/v1/me", "not-a-token", ""); code != 401 {
		t.Fatalf("me with bad token: %d", code)
	}

	if code, _ = do(t, "POST", srv.URL+"/v1/auth/verify-email", "", `{"token":"`+m.LastToken()+`"}`); code != 204 {
		t.Fatalf("verify: %d", code)
	}
	if code, body = do(t, "POST", srv.URL+"/v1/auth/verify-email", "", `{"token":"`+m.LastToken()+`"}`); code != 400 || errCode(body) != "bad_token" {
		t.Fatalf("verify reuse: %d %v", code, body)
	}

	if code, _ = do(t, "POST", srv.URL+"/v1/auth/logout", tok, ""); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	if code, _ = do(t, "GET", srv.URL+"/v1/me", tok, ""); code != 401 {
		t.Fatalf("me after logout: %d", code)
	}
}

func TestSignupAndLoginErrors(t *testing.T) {
	srv, _ := newAuthServer(t)
	signup := `{"email":"a@example.com","password":"a long passphrase","name":"A"}`
	if code, _ := do(t, "POST", srv.URL+"/v1/auth/signup", "", signup); code != 201 {
		t.Fatalf("signup: %d", code)
	}
	cases := []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"taken", "/v1/auth/signup", signup, 409, "email_taken"},
		{"short", "/v1/auth/signup", `{"email":"b@example.com","password":"short","name":"B"}`, 422, "password_too_short"},
		{"bad email", "/v1/auth/signup", `{"email":"nope","password":"a long passphrase","name":"B"}`, 400, "bad_request"},
		{"wrong password", "/v1/auth/login", `{"email":"a@example.com","password":"wrong one"}`, 401, "bad_credentials"},
		{"no such user", "/v1/auth/login", `{"email":"z@example.com","password":"wrong one"}`, 401, "bad_credentials"},
	}
	for _, c := range cases {
		code, body := do(t, "POST", srv.URL+c.path, "", c.body)
		if code != c.status || errCode(body) != c.code {
			t.Errorf("%s: got %d %v, want %d %s", c.name, code, body, c.status, c.code)
		}
	}
	code, body := do(t, "POST", srv.URL+"/v1/auth/login", "", `{"email":"A@example.com","password":"a long passphrase"}`)
	if user, _ := body["user"].(map[string]any); code != 200 || body["session_token"] == "" || user["mfa"] != false {
		t.Fatalf("login: %d %v", code, body)
	}
}

func TestBadPathParamIs400(t *testing.T) {
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{}))
	defer srv.Close()
	code, body := do(t, "POST", srv.URL+"/v1/auth/passkeys/login/not-a-uuid", "", `{}`)
	if code != 400 || errCode(body) != "bad_request" || body["error"].(map[string]any)["message"] != "That request couldn't be read." {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestOversizedBodyIs400(t *testing.T) {
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{}))
	defer srv.Close()
	big := `{"email":"a@example.com","password":"` + strings.Repeat("x", 2<<20) + `","name":"A"}`
	if code, body := do(t, "POST", srv.URL+"/v1/auth/signup", "", big); code != 400 || errCode(body) != "bad_request" {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestRateLimitAndSessionIPUseRealClientIP(t *testing.T) {
	pool := testdb.New(t)
	svc := auth.NewService(pool, &mailer.Memory{}, auth.Options{Origin: "http://localhost:5173", SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	cfg := config.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Pool: pool, Auth: svc, Config: cfg}))
	defer srv.Close()

	// Short passwords are rejected after the rate limit but before any hashing.
	short := `{"email":"a@example.com","password":"short","name":"A"}`
	for i := 0; i < 20; i++ {
		if code, _ := doFrom(t, "203.0.113.1", "POST", srv.URL+"/v1/auth/signup", "", short); code != 422 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if code, body := doFrom(t, "203.0.113.1", "POST", srv.URL+"/v1/auth/signup", "", short); code != 429 || errCode(body) != "rate_limited" {
		t.Fatalf("21st from one client: %d %v", code, body)
	}
	ok := `{"email":"b@example.com","password":"a long passphrase","name":"B"}`
	if code, body := doFrom(t, "203.0.113.2", "POST", srv.URL+"/v1/auth/signup", "", ok); code != 201 {
		t.Fatalf("another client behind the same proxy: %d %v", code, body)
	}
	var ip string
	if err := pool.QueryRow(context.Background(), `SELECT host(ip) FROM sessions`).Scan(&ip); err != nil || ip != "203.0.113.2" {
		t.Fatalf("sessions.ip = %q, %v", ip, err)
	}
}

func TestTOTPEndpoints(t *testing.T) {
	srv, _ := newAuthServer(t)
	_, body := do(t, "POST", srv.URL+"/v1/auth/signup", "", `{"email":"a@example.com","password":"a long passphrase","name":"A"}`)
	tok := body["session_token"].(string)
	if st, _ := do(t, "POST", srv.URL+"/v1/auth/totp", "", ""); st != 401 {
		t.Fatalf("unauthenticated start: %d", st)
	}
	if st, _ := do(t, "POST", srv.URL+"/v1/auth/totp", tok, `{"password":"wrong password!"}`); st != 401 {
		t.Fatalf("wrong password: %d", st)
	}
	st, setup := do(t, "POST", srv.URL+"/v1/auth/totp", tok, `{"password":"a long passphrase"}`)
	if st != 200 || setup["secret"] == "" || setup["otpauth_url"] == "" {
		t.Fatalf("start: %d %v", st, setup)
	}
	if st, _ := do(t, "POST", srv.URL+"/v1/auth/totp/confirm", tok, `{"code":"000000"}`); st != 400 {
		t.Fatalf("wrong code: %d", st)
	}
	code, _ := totp.GenerateCode(setup["secret"].(string), time.Now())
	if st, _ := do(t, "POST", srv.URL+"/v1/auth/totp/confirm", tok, `{"code":"`+code+`"}`); st != 204 {
		t.Fatalf("confirm: %d", st)
	}
	if st, e := do(t, "POST", srv.URL+"/v1/auth/totp", tok, `{"password":"a long passphrase"}`); st != 409 {
		t.Fatalf("restart while enabled: %d %v", st, e)
	}
}
