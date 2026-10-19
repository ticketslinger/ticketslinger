package mailer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestLogLeavesLinksOut(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	text := "Your tickets: http://localhost:3001/orders/TS-7K2QF?t=sekret1 and sign in at https://app.example/verify?token=sekret2."
	if err := (Log{}).Send(context.Background(), Message{To: "a@example.com", Subject: "Hi", Text: text}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "sekret") || strings.Contains(out, "http") || strings.Count(out, "[link removed]") != 2 || !strings.Contains(out, "Your tickets") {
		t.Fatalf("logged %q, want the text without its links", out)
	}
}

func TestLastLinkSkipsMessagesWithoutOne(t *testing.T) {
	m := &Memory{}
	ctx := context.Background()
	_ = m.Send(ctx, Message{To: "a@example.com", Subject: "Tickets", Text: "Here: http://localhost:3001/orders/TS-7K2QF?t=abc"})
	_ = m.Send(ctx, Message{To: "b@example.com", Subject: "Other", Text: "Here: http://localhost:3001/orders/TS-AAAAA?t=zzz"})
	_ = m.Send(ctx, Message{To: "a@example.com", Subject: "Refund", Text: "We sent $25.00 back."})
	if got := m.LastLink("a@example.com"); got != "http://localhost:3001/orders/TS-7K2QF?t=abc" {
		t.Fatalf("LastLink = %q, want the earlier message's link", got)
	}
	if got := m.LastLink("c@example.com"); got != "" {
		t.Fatalf("LastLink for a stranger = %q", got)
	}
}

func TestLogMailerShowsNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)
	err := (Log{}).Send(context.Background(), Message{
		To: "a@example.com", Subject: "Your tickets",
		Text:           "Open https://x.test/orders/TS-AAAAA?t=SECRET1 to see them.",
		HTML:           "<p>SECRET2</p>",
		Parts:          []Part{{Filename: "tickets.pdf", ContentType: "application/pdf", Data: []byte("SECRET3")}},
		IdempotencyKey: "SECRET4",
		Tags:           map[string]string{"order": "SECRET5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "[link removed]") || !strings.Contains(out, `"parts":1`) {
		t.Fatalf("logged %q, want the stripped text and a part count", out)
	}
	for _, s := range []string{"SECRET1", "SECRET2", "SECRET3", "SECRET4", "SECRET5"} {
		if strings.Contains(out, s) {
			t.Fatalf("logged %s: %q", s, out)
		}
	}
}

func TestErrorKeepsOnlyStatusAndCode(t *testing.T) {
	e := &Error{Provider: "resend", Status: 422, Code: "validation_error", Problem: "bad_address"}
	if got := e.Error(); got != "resend: 422 validation_error" {
		t.Fatalf("Error() = %q", got)
	}
	if got := (&Error{Provider: "smtp", Code: "starttls"}).Error(); got != "smtp: starttls" {
		t.Fatalf("Error() without a status = %q", got)
	}
	if got := ProblemOf(fmt.Errorf("x: %w", e)); got != "bad_address" {
		t.Fatalf("ProblemOf(wrapped) = %q", got)
	}
	if got := ProblemOf(io.EOF); got != "unreachable" {
		t.Fatalf("ProblemOf(io.EOF) = %q", got)
	}
}
