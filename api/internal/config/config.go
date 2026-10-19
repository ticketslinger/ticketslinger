// Package config reads runtime settings from TS_* environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL     string // ts_app
	HTTPAddr        string
	PublicURL       string
	DashboardOrigin string
	WebAuthnRPID    string
	// SecretKey encrypts secrets at rest (TS_SECRET_KEY: base64 of 32 bytes).
	// Nil when unset; `serve` refuses to start without it.
	SecretKey []byte
	// TrustedProxies may set the client address with X-Real-IP.
	TrustedProxies []netip.Prefix
	// PaymentsMode is TS_PAYMENTS: "connect" (default) charges on each
	// organizer's connected Stripe account; "direct" charges on the server's
	// own, with no Connect and no platform fee (self-hosting).
	PaymentsMode string
	// Platform fee per paid ticket: basis points of the pre-tax price plus a flat amount.
	PlatformFeeBP    int   // TS_PLATFORM_FEE_BP, 0-10000, default 200
	PlatformFeeCents int64 // TS_PLATFORM_FEE_CENTS, 0-10000, default 50
	// Stripe Connect. Without a secret key, paid tickets can't be sold.
	StripeSecretKey      string // TS_STRIPE_SECRET_KEY: sk_test_, sk_live_ or a restricted rk_ key
	StripePublishableKey string // TS_STRIPE_PUBLISHABLE_KEY: pk_, same mode; required with a secret key
	StripeWebhookSecret  string // TS_STRIPE_WEBHOOK_SECRET: whsec_; required with a secret key
	// StripeClientID turns on linking existing Stripe accounts (Connect
	// OAuth). Optional; needs a secret key.
	StripeClientID string // TS_STRIPE_CLIENT_ID: ca_
	StorefrontURL  string // TS_STOREFRONT_URL
	// Cloudflare Turnstile on sign-up, repeated failed sign-ins and checkouts. Off when the secret is unset.
	TurnstileSecret    string // TS_TURNSTILE_SECRET
	TurnstileVerifyURL string // TS_TURNSTILE_VERIFY_URL, Cloudflare's siteverify by default
	// Mail is TS_MAIL: "resend", "smtp", or "log" (only on a local TS_PUBLIC_URL).
	Mail                string
	MailFrom            string // TS_MAIL_FROM, parsed with net/mail
	MailAlertTo         string // TS_MAIL_ALERT_TO: quota warnings; optional
	ResendAPIKey        string // TS_RESEND_API_KEY: re_
	ResendAPIURL        string // TS_RESEND_API_URL: https://, or http on localhost for tests
	ResendWebhookSecret string // TS_RESEND_WEBHOOK_SECRET: whsec_; optional
	SMTPHost            string // TS_SMTP_HOST: required with TS_MAIL=smtp
	SMTPPort            int    // TS_SMTP_PORT: 587 by default; 465 is implicit TLS
	SMTPUsername        string // TS_SMTP_USERNAME and TS_SMTP_PASSWORD: both or neither
	SMTPPassword        string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:     os.Getenv("TS_DATABASE_URL"),
		HTTPAddr:        env("TS_HTTP_ADDR", "127.0.0.1:3000"),
		PublicURL:       env("TS_PUBLIC_URL", "http://localhost:3000"),
		DashboardOrigin: env("TS_DASHBOARD_ORIGIN", "http://localhost:5173"),
		WebAuthnRPID:    env("TS_WEBAUTHN_RPID", "localhost"),
		StorefrontURL:   env("TS_STOREFRONT_URL", "http://localhost:3001"),

		StripeSecretKey:      os.Getenv("TS_STRIPE_SECRET_KEY"),
		StripePublishableKey: os.Getenv("TS_STRIPE_PUBLISHABLE_KEY"),
		StripeWebhookSecret:  os.Getenv("TS_STRIPE_WEBHOOK_SECRET"),
		StripeClientID:       os.Getenv("TS_STRIPE_CLIENT_ID"),

		TurnstileSecret:    os.Getenv("TS_TURNSTILE_SECRET"),
		TurnstileVerifyURL: env("TS_TURNSTILE_VERIFY_URL", "https://challenges.cloudflare.com/turnstile/v0/siteverify"),

		Mail:                os.Getenv("TS_MAIL"),
		MailFrom:            env("TS_MAIL_FROM", "TicketSlinger <tickets@ticketslinger.com>"),
		MailAlertTo:         os.Getenv("TS_MAIL_ALERT_TO"),
		ResendAPIKey:        os.Getenv("TS_RESEND_API_KEY"),
		ResendAPIURL:        env("TS_RESEND_API_URL", "https://api.resend.com"),
		ResendWebhookSecret: os.Getenv("TS_RESEND_WEBHOOK_SECRET"),
		SMTPHost:            os.Getenv("TS_SMTP_HOST"),
		SMTPUsername:        os.Getenv("TS_SMTP_USERNAME"),
		SMTPPassword:        os.Getenv("TS_SMTP_PASSWORD"),
	}
	c.SMTPPort, _ = strconv.Atoi(env("TS_SMTP_PORT", "587")) // not a number: 0, refused with TS_MAIL=smtp
	for _, cidr := range strings.Split(env("TS_TRUSTED_PROXIES", "127.0.0.1/32,::1/128"), ",") {
		if cidr = strings.TrimSpace(cidr); cidr == "" {
			continue
		}
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return c, fmt.Errorf("TS_TRUSTED_PROXIES: %w", err)
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
	}
	if v := os.Getenv("TS_SECRET_KEY"); v != "" {
		k, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(k) != 32 {
			return c, errors.New("TS_SECRET_KEY must be base64 of exactly 32 bytes")
		}
		c.SecretKey = k
	}
	bpv, err := intEnv("TS_PLATFORM_FEE_BP", 200, 10000)
	if err != nil {
		return c, err
	}
	cents, err := intEnv("TS_PLATFORM_FEE_CENTS", 50, 10000)
	if err != nil {
		return c, err
	}
	c.PlatformFeeBP, c.PlatformFeeCents = bpv, int64(cents)
	if err := checkStripe(c); err != nil {
		return c, err
	}
	switch c.PaymentsMode = env("TS_PAYMENTS", "connect"); c.PaymentsMode {
	case "connect":
	case "direct":
		if err := checkDirect(c); err != nil {
			return c, err
		}
		c.PlatformFeeBP, c.PlatformFeeCents = 0, 0
	default:
		return c, errors.New("TS_PAYMENTS must be connect or direct")
	}
	if err := checkMail(&c); err != nil {
		return c, err
	}
	if c.DatabaseURL == "" {
		return c, errors.New("TS_DATABASE_URL is required")
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// intEnv reads an integer in [0, hi], or def when unset.
func intEnv(k string, def, hi int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > hi {
		return 0, fmt.Errorf("%s must be a whole number from 0 to %d", k, hi)
	}
	return n, nil
}

var clientIDRE = regexp.MustCompile(`^ca_[A-Za-z0-9]{1,250}$`)

// checkStripe checks the Stripe keys' shapes. Errors never repeat a key.
func checkStripe(c Config) error {
	sk, pk, wh := c.StripeSecretKey, c.StripePublishableKey, c.StripeWebhookSecret
	if wh != "" && !strings.HasPrefix(wh, "whsec_") {
		return errors.New("TS_STRIPE_WEBHOOK_SECRET must start with whsec_")
	}
	if id := c.StripeClientID; id != "" && !clientIDRE.MatchString(id) {
		return errors.New("TS_STRIPE_CLIENT_ID must be a Connect client id: ca_ followed by letters and digits")
	}
	if sk == "" {
		if pk != "" {
			return errors.New("TS_STRIPE_PUBLISHABLE_KEY is set without TS_STRIPE_SECRET_KEY")
		}
		if c.StripeClientID != "" {
			return errors.New("TS_STRIPE_CLIENT_ID is set without TS_STRIPE_SECRET_KEY")
		}
		return nil
	}
	skMode, ok := StripeMode(sk, "sk_", "rk_")
	if !ok {
		return errors.New("TS_STRIPE_SECRET_KEY must be a secret key (sk_test_, sk_live_) or a restricted key (rk_)")
	}
	pkMode, ok := StripeMode(pk, "pk_")
	if !ok {
		return errors.New("TS_STRIPE_PUBLISHABLE_KEY must be set with a secret key, and start with pk_test_ or pk_live_")
	}
	if skMode != pkMode {
		return errors.New("TS_STRIPE_SECRET_KEY and TS_STRIPE_PUBLISHABLE_KEY must both be test keys or both be live keys")
	}
	if wh == "" {
		return errors.New("TS_STRIPE_WEBHOOK_SECRET is required with a secret key")
	}
	return nil
}

// checkDirect: selling through the server's own Stripe account needs its
// keys, and has no Connect and no platform fee. Errors never repeat a key.
func checkDirect(c Config) error {
	switch {
	case c.StripeSecretKey == "":
		return errors.New("TS_PAYMENTS=direct needs TS_STRIPE_SECRET_KEY, TS_STRIPE_PUBLISHABLE_KEY and TS_STRIPE_WEBHOOK_SECRET")
	case c.StripeClientID != "":
		return errors.New("TS_PAYMENTS=direct takes no TS_STRIPE_CLIENT_ID: there are no connected accounts to link")
	case c.PlatformFeeBP != 0 && os.Getenv("TS_PLATFORM_FEE_BP") != "":
		return errors.New("TS_PAYMENTS=direct takes no platform fee: leave TS_PLATFORM_FEE_BP unset or 0")
	case c.PlatformFeeCents != 0 && os.Getenv("TS_PLATFORM_FEE_CENTS") != "":
		return errors.New("TS_PAYMENTS=direct takes no platform fee: leave TS_PLATFORM_FEE_CENTS unset or 0")
	}
	return nil
}

// checkMail picks the mailer and checks its settings. The log mailer sends
// nothing, so it is only allowed where nobody is waiting for real email: a
// local http TS_PUBLIC_URL. Errors never repeat a key or password.
func checkMail(c *Config) error {
	u, err := url.Parse(c.PublicURL)
	local := err == nil && u.Scheme == "http" && IsLocalHost(u.Hostname())
	switch c.Mail {
	case "":
		if !local {
			return errors.New("TS_MAIL is required: resend or smtp")
		}
		c.Mail = "log"
	case "log":
		if !local {
			return errors.New("TS_MAIL=log only works on a local TS_PUBLIC_URL; production needs resend or smtp")
		}
	case "resend":
		if !strings.HasPrefix(c.ResendAPIKey, "re_") {
			return errors.New("TS_RESEND_API_KEY must be set with TS_MAIL=resend and start with re_")
		}
	case "smtp":
		switch {
		case c.SMTPHost == "":
			return errors.New("TS_SMTP_HOST is required with TS_MAIL=smtp")
		case c.SMTPPort < 1 || c.SMTPPort > 65535:
			return errors.New("TS_SMTP_PORT must be a port number from 1 to 65535")
		case (c.SMTPUsername == "") != (c.SMTPPassword == ""):
			return errors.New("TS_SMTP_USERNAME and TS_SMTP_PASSWORD go together")
		}
	default:
		return errors.New("TS_MAIL must be resend, smtp or log")
	}
	if _, err := mail.ParseAddress(c.MailFrom); err != nil {
		return errors.New("TS_MAIL_FROM must be an address like TicketSlinger <tickets@example.com>")
	}
	if c.MailAlertTo != "" {
		if _, err := mail.ParseAddress(c.MailAlertTo); err != nil {
			return errors.New("TS_MAIL_ALERT_TO must be an address like you@example.com")
		}
	}
	// A typo here would send the bearer key in clear.
	if a, err := url.Parse(c.ResendAPIURL); err != nil || a.Hostname() == "" || a.Scheme != "https" && (a.Scheme != "http" || !IsLocalHost(a.Hostname())) {
		return errors.New("TS_RESEND_API_URL must be https:// unless it points at localhost")
	}
	// Checked as the webhook reads it, so a bad paste fails here, not on
	// every event. The error never repeats the value.
	if s := c.ResendWebhookSecret; s != "" {
		b64, ok := strings.CutPrefix(s, "whsec_")
		if key, err := base64.StdEncoding.DecodeString(b64); !ok || err != nil || len(key) == 0 {
			return errors.New("TS_RESEND_WEBHOOK_SECRET isn't a valid signing secret")
		}
	}
	return nil
}

// IsLocalHost reports whether host is "localhost" or a loopback IP.
func IsLocalHost(host string) bool {
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || err == nil && ip.IsLoopback()
}

// StripeMode is "test" or "live" for a key that starts with one of prefixes
// followed by test_ or live_.
func StripeMode(key string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		for _, mode := range []string{"test", "live"} {
			if strings.HasPrefix(key, p+mode+"_") {
				return mode, true
			}
		}
	}
	return "", false
}
