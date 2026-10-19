// Package mailer sends transactional email.
package mailer

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"sync"
)

// Part is a file sent with a message.
type Part struct {
	Filename, ContentType string
	ContentID             string // set: inline image referenced as cid:<ContentID>; empty: attachment
	Data                  []byte
}

// Message is one email. Text is always set; HTML is optional.
type Message struct {
	To, Subject, Text, HTML string
	Parts                   []Part
	IdempotencyKey          string            // Resend only; "" for none
	Tags                    map[string]string // Resend only; ASCII [A-Za-z0-9_-]
}

type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Kind says what a failed send should do next.
type Kind int

const (
	Retry      Kind = iota // try again later
	Permanent              // no retry can fix it
	DailyQuota             // wait for the provider's day to roll over
)

// Error is a provider's answer, cut down to what is safe to keep: provider
// messages can quote addresses, so only the status and code survive.
type Error struct {
	Provider string
	Kind     Kind
	Status   int
	Code     string
	Problem  string // rate_limited, quota, refused, unreachable, bad_address or config
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Provider + ": " + e.Code
	}
	return e.Provider + ": " + strconv.Itoa(e.Status) + " " + e.Code
}

// ProblemOf is the Problem of the *Error in err's chain, else "unreachable".
func ProblemOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Problem
	}
	return "unreachable"
}

// Log writes messages to the log instead of sending them. Real delivery
// comes later. Links are cut out first: a sign-in or order link is a key,
// and logs are kept and read far more widely than inboxes.
type Log struct{}

// HTML, part data, tags and the key never reach the log.
func (Log) Send(ctx context.Context, m Message) error {
	slog.InfoContext(ctx, "email", "to", m.To, "subject", m.Subject, "text", linkRe.ReplaceAllString(m.Text, "[link removed]"), "parts", len(m.Parts))
	return nil
}

// Memory keeps messages for tests.
type Memory struct {
	mu   sync.Mutex
	Sent []Message
}

func (m *Memory) Send(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = append(m.Sent, msg)
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

// LastLink returns the first link in the newest message sent to to that has
// one, or "". Messages without a link (a refund notice) are skipped.
func (m *Memory) LastLink(to string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.Sent) - 1; i >= 0; i-- {
		if m.Sent[i].To != to {
			continue
		}
		if l := linkRe.FindString(m.Sent[i].Text); l != "" {
			return l
		}
	}
	return ""
}
