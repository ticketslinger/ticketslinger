package mailer_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/jobs"
	"github.com/ticketslinger/ticketslinger/api/internal/mailer"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// answer is a fake Resend that replies with status and a body naming code.
func answer(t *testing.T, status int, code string, header ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		if status == 200 {
			_, _ = io.WriteString(w, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"statusCode": status, "name": code, "message": "something went wrong"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func provider(url string) *mailer.Resend {
	return &mailer.Resend{Key: "re_test_key", From: "TicketSlinger <tickets@ticketslinger.com>", URL: url}
}

func TestResendSendsTheMessage(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		_, _ = io.WriteString(w, `{"id":"x"}`)
	}))
	defer srv.Close()
	png, pdf := []byte{0x89, 'P', 'N', 'G', 0, 1, 2}, []byte("%PDF-1.7 \x00\xff")
	err := provider(srv.URL).Send(context.Background(), mailer.Message{
		To: "buyer@example.com", Subject: "Your tickets", Text: "Hello", HTML: `<p>Hello <img src="cid:qr-1" alt="QR"></p>`,
		Parts: []mailer.Part{
			{Filename: "qr-1.png", ContentType: "image/png", ContentID: "qr-1", Data: png},
			{Filename: "tickets.pdf", ContentType: "application/pdf", Data: pdf},
		},
		Tags: map[string]string{"order": "0b6f2c1e-6d7a-4f4e-9f2a-2f4c8d1e5a10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != "POST" || got.URL.Path != "/emails" || got.Header.Get("Authorization") != "Bearer re_test_key" ||
		got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request %s %s auth=%q type=%q", got.Method, got.URL.Path, got.Header.Get("Authorization"), got.Header.Get("Content-Type"))
	}
	var sent struct {
		From, Subject, HTML, Text string
		To                        []string
		Attachments               []map[string]string
		Tags                      []map[string]string
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.From != "TicketSlinger <tickets@ticketslinger.com>" || len(sent.To) != 1 || sent.To[0] != "buyer@example.com" ||
		sent.Subject != "Your tickets" || sent.Text != "Hello" || !strings.Contains(sent.HTML, "cid:qr-1") {
		t.Fatalf("body %s", body)
	}
	if len(sent.Attachments) != 2 {
		t.Fatalf("attachments %v", sent.Attachments)
	}
	inline, file := sent.Attachments[0], sent.Attachments[1]
	if inline["content_id"] != "qr-1" || inline["filename"] != "qr-1.png" || inline["content_type"] != "image/png" ||
		!bytes.Equal(must(base64.StdEncoding.DecodeString(inline["content"])), png) {
		t.Fatalf("inline part %v", inline)
	}
	if _, ok := file["content_id"]; ok || file["filename"] != "tickets.pdf" || file["content_type"] != "application/pdf" ||
		!bytes.Equal(must(base64.StdEncoding.DecodeString(file["content"])), pdf) {
		t.Fatalf("attachment %v", file)
	}
	if len(sent.Tags) != 1 || sent.Tags[0]["name"] != "order" || sent.Tags[0]["value"] != "0b6f2c1e-6d7a-4f4e-9f2a-2f4c8d1e5a10" {
		t.Fatalf("tags %v", sent.Tags)
	}

	// Without parts or tags, those arrays are left out.
	if err := provider(srv.URL).Send(context.Background(), mailer.Message{To: "a@example.com", Subject: "Hi", Text: "Hi"}); err != nil {
		t.Fatal(err)
	}
	if s := string(body); strings.Contains(s, "attachments") || strings.Contains(s, "tags") {
		t.Fatalf("body %s, want no empty arrays", s)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestResendSendsTheIdempotencyKey(t *testing.T) {
	var keys []string
	var present []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := r.Header["Idempotency-Key"]
		keys, present = append(keys, r.Header.Get("Idempotency-Key")), append(present, ok)
	}))
	defer srv.Close()
	ctx := context.Background()
	_ = provider(srv.URL).Send(ctx, mailer.Message{To: "a@example.com", Subject: "Hi", Text: "Hi", IdempotencyKey: "ts-job-42"})
	_ = provider(srv.URL).Send(ctx, mailer.Message{To: "a@example.com", Subject: "Hi", Text: "Hi"})
	if len(keys) != 2 || keys[0] != "ts-job-42" || !present[0] || present[1] {
		t.Fatalf("keys %q present %v", keys, present)
	}
}

func TestResendErrorMapping(t *testing.T) {
	for _, c := range []struct {
		status  int
		code    string
		kind    mailer.Kind
		problem string // "" means the send counts as done
	}{
		{200, "", 0, ""},
		{409, "invalid_idempotent_request", 0, ""},
		{409, "concurrent_idempotent_requests", mailer.Retry, "rate_limited"},
		{429, "rate_limit_exceeded", mailer.Retry, "rate_limited"},
		{429, "daily_quota_exceeded", mailer.DailyQuota, "quota"},
		{429, "monthly_quota_exceeded", mailer.Retry, "quota"},
		{401, "missing_api_key", mailer.Retry, "config"},
		{403, "invalid_api_key", mailer.Retry, "config"},
		{403, "validation_error", mailer.Retry, "config"},
		{404, "not_found", mailer.Retry, "config"},
		{422, "invalid_from_address", mailer.Retry, "config"},
		{400, "validation_error", mailer.Permanent, "bad_address"},
		{422, "validation_error", mailer.Permanent, "bad_address"},
		{422, "invalid_attachment", mailer.Permanent, "refused"},
		{500, "internal_server_error", mailer.Retry, "unreachable"},
		{503, "application_error", mailer.Retry, "unreachable"},
	} {
		err := provider(answer(t, c.status, c.code).URL).Send(context.Background(), mailer.Message{To: "a@example.com", Subject: "Hi", Text: "Hi"})
		if c.problem == "" {
			if err != nil {
				t.Errorf("%d %s: %v, want sent", c.status, c.code, err)
			}
			continue
		}
		e, ok := errors.AsType[*mailer.Error](err)
		if !ok || e.Provider != "resend" || e.Kind != c.kind || e.Problem != c.problem || e.Status != c.status || e.Code != c.code {
			t.Errorf("%d %s: %#v, want kind %d problem %s", c.status, c.code, e, c.kind, c.problem)
		}
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	err := provider(closed.URL).Send(context.Background(), mailer.Message{To: "a@example.com", Subject: "Hi", Text: "Hi"})
	if e, ok := errors.AsType[*mailer.Error](err); !ok || e.Kind != mailer.Retry || e.Problem != "unreachable" || strings.Contains(err.Error(), closed.URL) {
		t.Fatalf("closed server: %v", err)
	}
}

func TestResendErrorsKeepOnlyStatusAndCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		name := "validation_error"
		if r.Header.Get("Idempotency-Key") == "evil" {
			name = "Evil Code<x>"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"statusCode": 422, "name": name, "message": "Invalid `to`: buyer@example.com (key re_test_key)"})
	}))
	defer srv.Close()
	err := provider(srv.URL).Send(context.Background(), mailer.Message{To: "buyer@example.com", Subject: "Hi", Text: "Hi"})
	if err == nil || err.Error() != "resend: 422 validation_error" || strings.Contains(err.Error(), "buyer@example.com") || strings.Contains(err.Error(), "re_test_key") {
		t.Fatalf("err %v", err)
	}
	err = provider(srv.URL).Send(context.Background(), mailer.Message{To: "buyer@example.com", Subject: "Hi", Text: "Hi", IdempotencyKey: "evil"})
	if err == nil || err.Error() != "resend: 422 unknown" {
		t.Fatalf("err %v", err)
	}
}

func TestResendReportsQuota(t *testing.T) {
	var daily, monthly int
	for _, c := range []struct {
		srv          *httptest.Server
		wantD, wantM int
	}{
		{answer(t, 200, "", "x-resend-daily-quota", "12", "x-resend-monthly-quota", "340"), 12, 340},
		{answer(t, 200, ""), -1, -1},
		{answer(t, 429, "daily_quota_exceeded", "x-resend-daily-quota", "100"), 100, -1},
	} {
		daily, monthly = -9, -9
		r := provider(c.srv.URL)
		r.OnQuota = func(_ context.Context, d, m int) { daily, monthly = d, m }
		_ = r.Send(context.Background(), mailer.Message{To: "a@example.com", Subject: "Hi", Text: "Hi"})
		if daily != c.wantD || monthly != c.wantM {
			t.Errorf("OnQuota(%d, %d), want (%d, %d)", daily, monthly, c.wantD, c.wantM)
		}
	}
}

func TestDailyQuotaWaitsForMidnightUTC(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	r := provider(answer(t, 429, "daily_quota_exceeded").URL)
	w := &jobs.Worker{Pool: pool, Handlers: map[string]jobs.Handler{"mail": func(ctx context.Context, _ []byte) error {
		return jobs.Deliver(ctx, r, mailer.Message{To: "a@example.com", Subject: "Hi", Text: "Hello"})
	}}}
	// The next 00:05 UTC, read on both sides of the run in case it passes.
	next := func() time.Time {
		n := time.Now().UTC()
		at := n.Truncate(24 * time.Hour).Add(5 * time.Minute)
		if !at.After(n) {
			at = at.AddDate(0, 0, 1)
		}
		return at
	}
	before := next()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return jobs.Enqueue(ctx, tx, "mail", 1) }); err != nil {
		t.Fatal(err)
	}
	if ran, err := w.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	var runAt time.Time
	var attempts int
	var lastError string
	if err := pool.QueryRow(ctx, `select run_at, attempts, last_error from jobs`).Scan(&runAt, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if after := next(); !runAt.Equal(before) && !runAt.Equal(after) || attempts != 0 || lastError != "resend: 429 daily_quota_exceeded" {
		t.Fatalf("run_at %v attempts %d last_error %q, want %v, 0, the quota code", runAt, attempts, lastError, before)
	}
}
