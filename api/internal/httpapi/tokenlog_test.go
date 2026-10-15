package httpapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/payments"
	"github.com/ticketslinger/ticketslinger/api/internal/plugin"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// panicky is the fake provider with a bug: making an intent panics, after
// running during (while the request's Idempotency-Key row is in flight).
type panicky struct {
	*payments.Fake
	during func()
}

func (p *panicky) CreatePaymentIntent(context.Context, plugin.PaymentIntentInput) (plugin.PaymentIntent, error) {
	p.during()
	panic("intent bug")
}

// lockedBuffer is a log sink the server's goroutines can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The checkout token is a bearer key: neither a bad request's log line, a
// panic's, nor an Idempotency-Key row may carry it.
func TestCheckoutTokenStaysOutOfLogsAndKeys(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := &panicky{Fake: payments.NewFake()}
	s := newShopWith(t, nil, p)
	owner := testdb.Owner(t, s.pool)
	// What a crash right now would leave in idempotency_keys for 2 minutes.
	var scopes []string
	p.during = func() {
		rows, _ := owner.Query(context.Background(), `select scope || ' ' || key from idempotency_keys`)
		defer rows.Close()
		for rows.Next() {
			var sc string
			_ = rows.Scan(&sc)
			scopes = append(scopes, sc)
		}
	}
	token, _ := s.must(s.send("", "", "POST", "/v1/checkouts", s.cart(s.f["ticketType"], 1, "")), 201)["token"].(string)
	if token == "" {
		t.Fatal("no token")
	}
	for _, body := range []string{`{"email":`, `{"email":5}`, `[]`} {
		if res := s.send("", uuid.NewString(), "POST", "/v1/checkouts/"+token+"/pay", body); res.StatusCode != 400 {
			t.Fatalf("malformed %s: %d, want 400", body, res.StatusCode)
		}
	}
	// A panic answers 500, which no operation documents: send it past the validating client.
	req, _ := http.NewRequest("POST", s.srv.URL+"/v1/checkouts/"+token+"/pay", strings.NewReader(`{"email":"buyer@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	res, err := http.DefaultClient.Do(req) // raw: a panic's 500 is in no operation's spec
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 500 {
		t.Fatalf("panic: %d, want 500", res.StatusCode)
	}

	got := logs.String()
	if !strings.Contains(got, "msg=panic") || !strings.Contains(got, "msg=\"bad request\"") {
		t.Fatalf("expected the bad requests and the panic in the log:\n%s", got)
	}
	if strings.Contains(got, token) {
		t.Fatalf("the log carries the checkout token:\n%s", got)
	}
	if !strings.Contains(got, "/v1/checkouts/{token}/pay") {
		t.Fatalf("the log should name the route:\n%s", got)
	}
	if len(scopes) != 1 || !strings.Contains(scopes[0], "anon POST /v1/checkouts/{token}/pay ") || strings.Contains(scopes[0], token) {
		t.Fatalf("in-flight idempotency rows %q: want one, by route and hash, without the token", scopes)
	}
	var leaks int
	if err := owner.QueryRow(context.Background(), `select count(*) from idempotency_keys
		where scope like '%' || $1 || '%' or key like '%' || $1 || '%' or convert_from(coalesce(body, ''), 'UTF8') like '%' || $1 || '%'`,
		token).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("%d idempotency rows carry the token (%v), want none", leaks, err)
	}
}
