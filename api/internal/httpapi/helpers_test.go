package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"

	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
)

const testPassword = "a long passphrase"

type harness struct {
	t    *testing.T
	srv  *httptest.Server
	mail *mailer.Memory
	pool *pgxpool.Pool // ts_app
}

func newHarness(t *testing.T) *harness {
	srv, m, pool := newAuthServerPool(t)
	return &harness{t, srv, m, pool}
}

// do sends a request with the session token (empty for none).
func (h *harness) do(token, method, path, body string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func (h *harness) json(res *http.Response) map[string]any {
	h.t.Helper()
	b, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func (h *harness) errorCode(res *http.Response) string {
	e, _ := h.json(res)["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (h *harness) must(res *http.Response, status int) map[string]any {
	h.t.Helper()
	b := h.json(res)
	if res.StatusCode != status {
		h.t.Fatalf("%s %s: got %d %v, want %d", res.Request.Method, res.Request.URL.Path, res.StatusCode, b, status)
	}
	return b
}

// signupUnverified returns a session token for an account that hasn't verified its email.
func (h *harness) signupUnverified(email string) string {
	h.t.Helper()
	b := h.must(h.do("", "POST", "/v1/auth/signup", `{"email":"`+email+`","password":"`+testPassword+`","name":"T"}`), 201)
	return b["session_token"].(string)
}

// signupVerified returns a session token with mfa=false.
func (h *harness) signupVerified(email string) string {
	h.t.Helper()
	tok := h.signupUnverified(email)
	h.must(h.do("", "POST", "/v1/auth/verify-email", `{"token":"`+h.mail.LastToken()+`"}`), 204)
	return tok
}

// signupWithMFA enrols an authenticator app, then signs in with a code: mfa=true.
func (h *harness) signupWithMFA(email string) string {
	h.t.Helper()
	tok := h.signupVerified(email)
	setup := h.must(h.do(tok, "POST", "/v1/auth/totp", `{"password":"`+testPassword+`"}`), 200)
	secret := setup["secret"].(string)
	code, _ := totp.GenerateCode(secret, time.Now())
	h.must(h.do(tok, "POST", "/v1/auth/totp/confirm", `{"code":"`+code+`"}`), 204)
	// Enrolling uses the current step; log in with the next one.
	code, _ = totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	b := h.must(h.do("", "POST", "/v1/auth/login", `{"email":"`+email+`","password":"`+testPassword+`","totp_code":"`+code+`"}`), 200)
	if b["user"].(map[string]any)["mfa"] != true {
		h.t.Fatalf("login did not give an mfa session: %v", b)
	}
	return b["session_token"].(string)
}

func (h *harness) createOrganizer(token, slug string) {
	h.t.Helper()
	h.must(h.do(token, "POST", "/v1/organizers", `{"name":"`+slug+`","slug":"`+slug+`"}`), 201)
}
