// Package turnstile checks Cloudflare Turnstile tokens with siteverify.
package turnstile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var (
	// ErrFailed: the token is missing, or Cloudflare says it isn't good.
	ErrFailed = errors.New("turnstile: check failed")
	// ErrUnavailable: siteverify couldn't answer. Callers refuse the attempt.
	ErrUnavailable = errors.New("turnstile: siteverify unavailable")
)

// Verifier checks tokens against siteverify. A nil Verifier, or one without
// a Secret, passes everything: Turnstile is off.
type Verifier struct {
	Secret, URL string
	Client      *http.Client
}

// Enabled reports whether tokens are checked at all.
func (v *Verifier) Enabled() bool { return v != nil && v.Secret != "" }

// Verify spends token. ip is the visitor's address, or "".
// Errors never carry the secret.
func (v *Verifier) Verify(ctx context.Context, token, ip string) error {
	if !v.Enabled() {
		return nil
	}
	if token == "" {
		return ErrFailed
	}
	form := url.Values{"secret": {v.Secret}, "response": {token}}
	if ip != "" {
		form.Set("remoteip", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := v.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = res.Body.Close() }()
	var out struct {
		Success bool `json:"success"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&out) != nil {
		return fmt.Errorf("%w: siteverify answered %d", ErrUnavailable, res.StatusCode)
	}
	if !out.Success {
		return ErrFailed
	}
	return nil
}
