// Package mailer sends transactional email.
package mailer

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"sync"
)

type Mailer interface {
	Send(ctx context.Context, to, subject, text string) error
}

// Log writes messages to the log instead of sending them. Real delivery comes later.
type Log struct{}

func (Log) Send(ctx context.Context, to, subject, text string) error {
	slog.InfoContext(ctx, "email", "to", to, "subject", subject, "text", text)
	return nil
}

type Message struct{ To, Subject, Text string }

// Memory keeps messages for tests.
type Memory struct {
	mu   sync.Mutex
	Sent []Message
}

func (m *Memory) Send(_ context.Context, to, subject, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = append(m.Sent, Message{to, subject, text})
	return nil
}

var linkRe = regexp.MustCompile(`https?://\S+`)

// LastToken returns the token= query value from the link in the last message.
func (m *Memory) LastToken() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Sent) == 0 {
		return ""
	}
	u, err := url.Parse(linkRe.FindString(m.Sent[len(m.Sent)-1].Text))
	if err != nil {
		return ""
	}
	return u.Query().Get("token")
}

// LastLink returns the first link in the newest message sent to to, or "".
func (m *Memory) LastLink(to string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.Sent) - 1; i >= 0; i-- {
		if m.Sent[i].To == to {
			return linkRe.FindString(m.Sent[i].Text)
		}
	}
	return ""
}
