package turnstile_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ticketslinger/ticketslinger/api/internal/turnstile"
)

const secret = "0x4AAAAAAAsecret-never-shown"

// siteverify fakes Cloudflare's endpoint: it records the form and answers
// with status and body.
func siteverify(t *testing.T, status int, body string, delay time.Duration) (*turnstile.Verifier, *http.Request) {
	t.Helper()
	got := &http.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*got = *r
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &turnstile.Verifier{Secret: secret, URL: srv.URL, Client: &http.Client{Timeout: 200 * time.Millisecond}}, got
}

func TestVerifyPostsTheForm(t *testing.T) {
	v, got := siteverify(t, 200, `{"success":true}`, 0)
	if err := v.Verify(context.Background(), "tok", "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
		got.PostForm.Get("secret") != secret || got.PostForm.Get("response") != "tok" || got.PostForm.Get("remoteip") != "203.0.113.9" {
		t.Fatalf("siteverify got %s %q %v", got.Method, got.Header.Get("Content-Type"), got.PostForm)
	}
	if err := v.Verify(context.Background(), "tok", ""); err != nil || got.PostForm.Has("remoteip") {
		t.Fatalf("unknown address: %v, form %v", err, got.PostForm)
	}
}

func TestVerifyRefusals(t *testing.T) {
	v, _ := siteverify(t, 200, `{"success":false,"error-codes":["invalid-input-response"]}`, 0)
	if err := v.Verify(context.Background(), "tok", ""); !errors.Is(err, turnstile.ErrFailed) {
		t.Fatalf("Cloudflare said no: %v, want ErrFailed", err)
	}
	if err := v.Verify(context.Background(), "", ""); !errors.Is(err, turnstile.ErrFailed) {
		t.Fatalf("empty token: %v, want ErrFailed", err)
	}
}

// An outage fails closed: an error that isn't ErrFailed (the API answers
// 503), never a pass, and never the secret.
func TestVerifyOutageFailsClosed(t *testing.T) {
	for name, v := range map[string]*turnstile.Verifier{
		"500":     func() *turnstile.Verifier { v, _ := siteverify(t, 500, `oops`, 0); return v }(),
		"garbage": func() *turnstile.Verifier { v, _ := siteverify(t, 200, `<html>`, 0); return v }(),
		"timeout": func() *turnstile.Verifier { v, _ := siteverify(t, 200, `{"success":true}`, time.Second); return v }(),
		"refused": {Secret: secret, URL: "http://127.0.0.1:1/siteverify", Client: http.DefaultClient},
	} {
		err := v.Verify(context.Background(), "tok", "")
		if !errors.Is(err, turnstile.ErrUnavailable) || errors.Is(err, turnstile.ErrFailed) {
			t.Errorf("%s: %v, want ErrUnavailable", name, err)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error repeats the secret: %v", name, err)
		}
	}
}

func TestVerifyIsOffWithoutASecret(t *testing.T) {
	var none *turnstile.Verifier
	if err := none.Verify(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := (&turnstile.Verifier{}).Verify(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if none.Enabled() || (&turnstile.Verifier{}).Enabled() || !(&turnstile.Verifier{Secret: "x"}).Enabled() {
		t.Fatal("Enabled")
	}
}
