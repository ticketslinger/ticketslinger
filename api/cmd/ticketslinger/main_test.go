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

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
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
		if _, err := newHandler(config.Config{HTTPAddr: addr}, nil, true); err == nil {
			t.Errorf("%s: test mode started on a non-loopback address", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:3000", "[::1]:3000", "localhost:3000"} {
		if _, err := newHandler(config.Config{HTTPAddr: addr}, nil, true); err != nil {
			t.Errorf("%s: %v", addr, err)
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
