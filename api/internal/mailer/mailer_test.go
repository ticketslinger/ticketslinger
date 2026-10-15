package mailer

import (
	"bytes"
	"context"
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
	if err := (Log{}).Send(context.Background(), "a@example.com", "Hi", text); err != nil {
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
	_ = m.Send(ctx, "a@example.com", "Tickets", "Here: http://localhost:3001/orders/TS-7K2QF?t=abc")
	_ = m.Send(ctx, "b@example.com", "Other", "Here: http://localhost:3001/orders/TS-AAAAA?t=zzz")
	_ = m.Send(ctx, "a@example.com", "Refund", "We sent $25.00 back.")
	if got := m.LastLink("a@example.com"); got != "http://localhost:3001/orders/TS-7K2QF?t=abc" {
		t.Fatalf("LastLink = %q, want the earlier message's link", got)
	}
	if got := m.LastLink("c@example.com"); got != "" {
		t.Fatalf("LastLink for a stranger = %q", got)
	}
}
