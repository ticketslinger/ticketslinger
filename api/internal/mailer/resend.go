package mailer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Resend sends through Resend's REST API (POST /emails) with the stdlib.
type Resend struct {
	Key, From, URL string
	Client         *http.Client // nil: a 15 s timeout
	// OnQuota gets Resend's used daily and monthly counts after every
	// answer; a missing header is -1. Resend sends both on a 2xx (the daily
	// one only on the free plan) and neither on a 422.
	OnQuota func(ctx context.Context, daily, monthly int)
}

type resendAttachment struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
	ContentID   string `json:"content_id,omitempty"`
}

type resendTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resendEmail struct {
	From        string             `json:"from"`
	To          []string           `json:"to"`
	Subject     string             `json:"subject"`
	HTML        string             `json:"html,omitempty"`
	Text        string             `json:"text"`
	Attachments []resendAttachment `json:"attachments,omitempty"`
	Tags        []resendTag        `json:"tags,omitempty"`
}

var resendCodeRe = regexp.MustCompile(`^[a-z_]{1,64}$`)

// defaultResendClient never follows a redirect: a 3xx is an answer, kept as
// unreachable, and the bearer key stays with the host it was meant for.
var defaultResendClient = &http.Client{Timeout: 15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (r *Resend) Send(ctx context.Context, m Message) error {
	body := resendEmail{From: r.From, To: []string{m.To}, Subject: m.Subject, HTML: m.HTML, Text: m.Text}
	for _, p := range m.Parts {
		body.Attachments = append(body.Attachments, resendAttachment{Filename: p.Filename, Content: base64.StdEncoding.EncodeToString(p.Data),
			ContentType: p.ContentType, ContentID: p.ContentID})
	}
	for k, v := range m.Tags {
		body.Tags = append(body.Tags, resendTag{k, v})
	}
	slices.SortFunc(body.Tags, func(a, b resendTag) int { return strings.Compare(a.Name, b.Name) })
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(r.URL, "/")+"/emails", bytes.NewReader(b))
	if err != nil {
		slog.ErrorContext(ctx, "resend: TS_RESEND_API_URL can't make a request; email waits until it is fixed")
		return &Error{Provider: "resend", Kind: Retry, Code: "config", Problem: "config"}
	}
	req.Header.Set("Authorization", "Bearer "+r.Key)
	req.Header.Set("Content-Type", "application/json")
	if m.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	}
	c := r.Client
	if c == nil {
		c = defaultResendClient
	}
	res, err := c.Do(req)
	if err != nil {
		// The transport's error names the URL and maybe the cause; neither is kept.
		return &Error{Provider: "resend", Kind: Retry, Code: "network", Problem: "unreachable"}
	}
	defer func() { _ = res.Body.Close() }()
	if r.OnQuota != nil {
		r.OnQuota(ctx, quotaHeader(res.Header, "x-resend-daily-quota"), quotaHeader(res.Header, "x-resend-monthly-quota"))
	}
	var answer struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&answer) // message is never read
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode/100 == 2 {
		return nil
	}
	code := answer.Name
	if !resendCodeRe.MatchString(code) {
		code = "unknown"
	}
	e := &Error{Provider: "resend", Kind: Retry, Status: res.StatusCode, Code: code}
	switch s := res.StatusCode; {
	case s == 409 && code == "invalid_idempotent_request":
		// Same key, different body: the first attempt already went.
		slog.WarnContext(ctx, "resend: idempotency key reused with a different body; counting it as sent", "status", s, "code", code)
		return nil
	case s == 409 && code == "concurrent_idempotent_requests", s == 429 && code == "rate_limit_exceeded":
		e.Problem = "rate_limited"
	case s == 429 && code == "daily_quota_exceeded":
		e.Kind, e.Problem = DailyQuota, "quota"
	case s == 429 && code == "monthly_quota_exceeded":
		e.Problem = "quota"
	case s == 401, s == 403, s == 404, s == 422 && code == "invalid_from_address":
		e.Problem = "config"
	case s == 400, s == 422:
		e.Kind, e.Problem = Permanent, "refused"
		if code == "validation_error" {
			e.Problem = "bad_address"
		}
	default:
		e.Problem = "unreachable"
	}
	if e.Problem == "config" || code == "monthly_quota_exceeded" {
		slog.ErrorContext(ctx, "resend refused the send; email waits until it is fixed", "status", e.Status, "code", code)
	}
	return e
}

func quotaHeader(h http.Header, name string) int {
	n, err := strconv.Atoi(h.Get(name))
	if err != nil {
		return -1
	}
	return n
}
