package config

import (
	"strings"
	"testing"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when TS_DATABASE_URL is empty")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != "127.0.0.1:3000" || c.WebAuthnRPID != "localhost" ||
		len(c.TrustedProxies) != 2 || c.TrustedProxies[0].String() != "127.0.0.1/32" || c.TrustedProxies[1].String() != "::1/128" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadRejectsBadTrustedProxy(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	t.Setenv("TS_TRUSTED_PROXIES", "10.0.0.0/8, nonsense")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for an invalid CIDR")
	}
}

func TestLoadSecretKey(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	t.Setenv("TS_SECRET_KEY", "")
	if c, err := Load(); err != nil || c.SecretKey != nil {
		t.Fatalf("unset key: %v %v", c.SecretKey, err)
	}
	t.Setenv("TS_SECRET_KEY", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if c, err := Load(); err != nil || len(c.SecretKey) != 32 {
		t.Fatalf("good key: %v", err)
	}
	for _, bad := range []string{"not base64!", "c2hvcnQ="} {
		t.Setenv("TS_SECRET_KEY", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestLoadPlatformFee(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	c, err := Load()
	if err != nil || c.PlatformFeeBP != 200 || c.PlatformFeeCents != 50 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	t.Setenv("TS_PLATFORM_FEE_BP", "0")
	t.Setenv("TS_PLATFORM_FEE_CENTS", "10000")
	if c, err = Load(); err != nil || c.PlatformFeeBP != 0 || c.PlatformFeeCents != 10000 {
		t.Fatalf("bounds: %+v %v", c, err)
	}
	for _, kv := range [][2]string{
		{"TS_PLATFORM_FEE_BP", "10001"}, {"TS_PLATFORM_FEE_BP", "-1"}, {"TS_PLATFORM_FEE_BP", "2.5"}, {"TS_PLATFORM_FEE_BP", "abc"},
		{"TS_PLATFORM_FEE_CENTS", "10001"}, {"TS_PLATFORM_FEE_CENTS", "-1"}, {"TS_PLATFORM_FEE_CENTS", "x"},
	} {
		t.Setenv("TS_PLATFORM_FEE_BP", "200")
		t.Setenv("TS_PLATFORM_FEE_CENTS", "50")
		t.Setenv(kv[0], kv[1])
		if _, err := Load(); err == nil {
			t.Fatalf("expected error for %s=%s", kv[0], kv[1])
		}
	}
}

func TestLoadStripe(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	set := func(sk, pk, wh string) {
		t.Setenv("TS_STRIPE_SECRET_KEY", sk)
		t.Setenv("TS_STRIPE_PUBLISHABLE_KEY", pk)
		t.Setenv("TS_STRIPE_WEBHOOK_SECRET", wh)
	}
	set("", "", "")
	c, err := Load()
	if err != nil || c.StripeSecretKey != "" || c.StorefrontURL != "http://localhost:3001" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, ok := range [][3]string{
		{"sk_test_abc", "pk_test_abc", "whsec_abc"},
		{"sk_live_abc", "pk_live_abc", "whsec_abc"},
		{"rk_test_abc", "pk_test_abc", "whsec_abc"},
		{"rk_live_abc", "pk_live_abc", "whsec_abc"},
		{"", "", "whsec_e2e"}, // test mode: the fake provider, signed webhooks
	} {
		set(ok[0], ok[1], ok[2])
		c, err := Load()
		if err != nil || c.StripeSecretKey != ok[0] || c.StripePublishableKey != ok[1] || c.StripeWebhookSecret != ok[2] {
			t.Fatalf("%v: %+v %v", ok, c, err)
		}
	}
	for _, bad := range [][3]string{
		{"sk_test_abc", "", "whsec_abc"},            // no publishable key
		{"sk_test_abc", "pk_test_abc", ""},          // no webhook secret
		{"pk_test_abc", "pk_test_abc", "whsec_abc"}, // not a secret key
		{"sk_test_abc", "sk_test_abc", "whsec_abc"}, // not a publishable key
		{"sk_test_abc", "pk_live_abc", "whsec_abc"}, // modes differ
		{"sk_live_abc", "pk_test_abc", "whsec_abc"},
		{"rk_live_abc", "pk_test_abc", "whsec_abc"},
		{"sk_test_abc", "pk_test_abc", "abc"}, // not a webhook secret
		{"", "", "abc"},
		{"", "pk_test_abc", "whsec_abc"}, // a publishable key on its own
	} {
		set(bad[0], bad[1], bad[2])
		_, err := Load()
		if err == nil {
			t.Fatalf("%v: expected an error", bad)
		}
		for _, v := range bad {
			if len(v) > 3 && strings.Contains(err.Error(), v) {
				t.Fatalf("%v: error %q repeats a key", bad, err)
			}
		}
	}
}

func TestLoadStripeClientID(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	t.Setenv("TS_STRIPE_SECRET_KEY", "sk_test_abc")
	t.Setenv("TS_STRIPE_PUBLISHABLE_KEY", "pk_test_abc")
	t.Setenv("TS_STRIPE_WEBHOOK_SECRET", "whsec_abc")
	t.Setenv("TS_STRIPE_CLIENT_ID", "")
	if c, err := Load(); err != nil || c.StripeClientID != "" {
		t.Fatalf("unset: %q %v", c.StripeClientID, err)
	}
	t.Setenv("TS_STRIPE_CLIENT_ID", "ca_ABC123def")
	if c, err := Load(); err != nil || c.StripeClientID != "ca_ABC123def" {
		t.Fatalf("set: %q %v", c.StripeClientID, err)
	}
	for _, bad := range []string{"ABC123def", "pk_test_SecretLooking", "ca_", "ca_has space", "ca_abc\n"} {
		t.Setenv("TS_STRIPE_CLIENT_ID", bad)
		_, err := Load()
		if err == nil {
			t.Fatalf("%q: expected an error", bad)
		}
		if len(bad) > 3 && strings.Contains(err.Error(), bad) {
			t.Fatalf("%q: error %q repeats the value", bad, err)
		}
	}
	t.Setenv("TS_STRIPE_CLIENT_ID", "ca_ABC123def")
	t.Setenv("TS_STRIPE_SECRET_KEY", "")
	t.Setenv("TS_STRIPE_PUBLISHABLE_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("a client id without a secret key: expected an error")
	}
}

func TestLoadTurnstile(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	t.Setenv("TS_TURNSTILE_SECRET", "")
	c, err := Load()
	if err != nil || c.TurnstileSecret != "" || c.TurnstileVerifyURL != "https://challenges.cloudflare.com/turnstile/v0/siteverify" {
		t.Fatalf("defaults: %q %q %v", c.TurnstileSecret, c.TurnstileVerifyURL, err)
	}
	t.Setenv("TS_TURNSTILE_SECRET", "1x0000000000000000000000000000000AA")
	t.Setenv("TS_TURNSTILE_VERIFY_URL", "http://127.0.0.1:9/siteverify")
	if c, err = Load(); err != nil || c.TurnstileSecret != "1x0000000000000000000000000000000AA" || c.TurnstileVerifyURL != "http://127.0.0.1:9/siteverify" {
		t.Fatalf("set: %q %q %v", c.TurnstileSecret, c.TurnstileVerifyURL, err)
	}
}

func TestLoadDirectPayments(t *testing.T) {
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	keys := func(sk string) {
		t.Setenv("TS_STRIPE_SECRET_KEY", sk)
		t.Setenv("TS_STRIPE_PUBLISHABLE_KEY", "pk_test_pubkey")
		t.Setenv("TS_STRIPE_WEBHOOK_SECRET", "whsec_hooksecret")
		t.Setenv("TS_STRIPE_CLIENT_ID", "")
		t.Setenv("TS_PLATFORM_FEE_BP", "")
		t.Setenv("TS_PLATFORM_FEE_CENTS", "")
	}
	t.Setenv("TS_PAYMENTS", "")
	keys("sk_test_secretkey")
	if c, err := Load(); err != nil || c.PaymentsMode != "connect" || c.PlatformFeeBP != 200 {
		t.Fatalf("default: %q %d %v", c.PaymentsMode, c.PlatformFeeBP, err)
	}
	t.Setenv("TS_PAYMENTS", "direct")
	c, err := Load()
	if err != nil || c.PaymentsMode != "direct" || c.PlatformFeeBP != 0 || c.PlatformFeeCents != 0 {
		t.Fatalf("direct: %q %d %d %v", c.PaymentsMode, c.PlatformFeeBP, c.PlatformFeeCents, err)
	}
	t.Setenv("TS_PLATFORM_FEE_BP", "0")
	t.Setenv("TS_PLATFORM_FEE_CENTS", "0")
	if c, err := Load(); err != nil || c.PlatformFeeBP != 0 || c.PlatformFeeCents != 0 {
		t.Fatalf("direct with fees set to 0: %+v %v", c, err)
	}
	for name, tc := range map[string]struct {
		set   func()
		names string
	}{
		"no secret key":     {func() { keys(""); t.Setenv("TS_STRIPE_PUBLISHABLE_KEY", "") }, "TS_STRIPE_SECRET_KEY"},
		"no webhook secret": {func() { keys("sk_test_secretkey"); t.Setenv("TS_STRIPE_WEBHOOK_SECRET", "") }, "TS_STRIPE_WEBHOOK_SECRET"},
		"a client id":       {func() { keys("sk_test_secretkey"); t.Setenv("TS_STRIPE_CLIENT_ID", "ca_ABC123def") }, "TS_STRIPE_CLIENT_ID"},
		"a percentage fee":  {func() { keys("sk_test_secretkey"); t.Setenv("TS_PLATFORM_FEE_BP", "200") }, "TS_PLATFORM_FEE_BP"},
		"a flat fee":        {func() { keys("sk_test_secretkey"); t.Setenv("TS_PLATFORM_FEE_CENTS", "50") }, "TS_PLATFORM_FEE_CENTS"},
	} {
		tc.set()
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), tc.names) {
			t.Fatalf("%s: %v, want an error naming %s", name, err, tc.names)
		}
		for _, v := range []string{"secretkey", "pubkey", "hooksecret", "ABC123def"} {
			if strings.Contains(err.Error(), v) {
				t.Fatalf("%s: error %q repeats a key", name, err)
			}
		}
	}
	keys("sk_test_secretkey")
	t.Setenv("TS_PAYMENTS", "sideways")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TS_PAYMENTS") {
		t.Fatalf("sideways: %v", err)
	}
}

func TestMailConfig(t *testing.T) {
	const prod = "https://tickets.ticketslinger.com"
	for _, c := range []struct {
		name, url, mail, key, from, apiURL, hook, alert string
		host, port, user, pass                          string
		want, wantErr                                   string
	}{
		{name: "local default", url: "http://localhost:3000", want: "log"},
		{name: "loopback default", url: "http://127.0.0.1:3001", want: "log"},
		{name: "production needs a choice", url: prod, wantErr: "TS_MAIL is required: resend or smtp"},
		{name: "production refuses log", url: prod, mail: "log", wantErr: "TS_MAIL=log only works on a local TS_PUBLIC_URL; production needs resend or smtp"},
		{name: "https localhost is not local http", url: "https://localhost:3000", mail: "log", wantErr: "TS_MAIL=log only works on a local TS_PUBLIC_URL"},
		{name: "resend", url: prod, mail: "resend", key: "re_abc", want: "resend"},
		{name: "resend without key", url: prod, mail: "resend", wantErr: "TS_RESEND_API_KEY must be set with TS_MAIL=resend and start with re_"},
		{name: "resend with a wrong key", url: prod, mail: "resend", key: "sk_x", wantErr: "TS_RESEND_API_KEY must be set with TS_MAIL=resend and start with re_"},
		{name: "bad from", url: prod, mail: "resend", key: "re_abc", from: "tickets at example", wantErr: "TS_MAIL_FROM must be an address like TicketSlinger <tickets@example.com>"},
		{name: "plain http api", url: prod, mail: "resend", key: "re_abc", apiURL: "http://api.resend.com", wantErr: "TS_RESEND_API_URL must be https:// unless it points at localhost"},
		{name: "loopback api", url: prod, mail: "resend", key: "re_abc", apiURL: "http://127.0.0.1:9999", want: "resend"},
		{name: "https api without a host", url: prod, mail: "resend", key: "re_abc", apiURL: "https:///emails", wantErr: "TS_RESEND_API_URL must be https:// unless it points at localhost"},
		{name: "https api", url: prod, mail: "resend", key: "re_abc", apiURL: "https://api.resend.com", want: "resend"},
		{name: "bad webhook secret", url: prod, mail: "resend", key: "re_abc", hook: "sec_x", wantErr: "TS_RESEND_WEBHOOK_SECRET must start with whsec_"},
		{name: "webhook secret", url: prod, mail: "resend", key: "re_abc", hook: "whsec_x", want: "resend"},
		{name: "bad alert address", url: prod, mail: "resend", key: "re_abc", alert: "nobody", wantErr: "TS_MAIL_ALERT_TO must be an address like you@example.com"},
		{name: "alert address", url: prod, mail: "resend", key: "re_abc", alert: "james@example.com", want: "resend"},
		{name: "unknown provider", url: prod, mail: "sendgrid", wantErr: "TS_MAIL must be resend, smtp or log"},
		{name: "smtp", url: prod, mail: "smtp", host: "smtp.example.com", user: "relay", pass: "hunter2-SECRET", want: "smtp"},
		{name: "smtp on 465 without auth", url: prod, mail: "smtp", host: "smtp.example.com", port: "465", want: "smtp"},
		{name: "smtp without host", url: prod, mail: "smtp", user: "relay", pass: "hunter2-SECRET", wantErr: "TS_SMTP_HOST is required with TS_MAIL=smtp"},
		{name: "smtp port zero", url: prod, mail: "smtp", host: "smtp.example.com", port: "0", pass: "hunter2-SECRET", wantErr: "TS_SMTP_PORT must be a port number from 1 to 65535"},
		{name: "smtp port too big", url: prod, mail: "smtp", host: "smtp.example.com", port: "65536", wantErr: "TS_SMTP_PORT must be a port number from 1 to 65535"},
		{name: "smtp port not a number", url: prod, mail: "smtp", host: "smtp.example.com", port: "submission", wantErr: "TS_SMTP_PORT must be a port number from 1 to 65535"},
		{name: "smtp password alone", url: prod, mail: "smtp", host: "smtp.example.com", pass: "hunter2-SECRET", wantErr: "TS_SMTP_USERNAME and TS_SMTP_PASSWORD go together"},
		{name: "smtp username alone", url: prod, mail: "smtp", host: "smtp.example.com", user: "relay", wantErr: "TS_SMTP_USERNAME and TS_SMTP_PASSWORD go together"},
		{name: "smtp bad from", url: prod, mail: "smtp", host: "smtp.example.com", user: "relay", pass: "hunter2-SECRET", from: "nobody", wantErr: "TS_MAIL_FROM must be an address"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TS_DATABASE_URL", "postgres://x")
			t.Setenv("TS_PUBLIC_URL", c.url)
			t.Setenv("TS_MAIL", c.mail)
			t.Setenv("TS_RESEND_API_KEY", c.key)
			t.Setenv("TS_MAIL_FROM", c.from)
			t.Setenv("TS_RESEND_API_URL", c.apiURL)
			t.Setenv("TS_RESEND_WEBHOOK_SECRET", c.hook)
			t.Setenv("TS_MAIL_ALERT_TO", c.alert)
			t.Setenv("TS_SMTP_HOST", c.host)
			t.Setenv("TS_SMTP_PORT", c.port)
			t.Setenv("TS_SMTP_USERNAME", c.user)
			t.Setenv("TS_SMTP_PASSWORD", c.pass)
			cfg, err := Load()
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err %v, want %q", err, c.wantErr)
				}
				if c.key != "" && strings.Contains(err.Error(), c.key) || c.pass != "" && strings.Contains(err.Error(), c.pass) {
					t.Fatalf("error %q repeats a secret", err)
				}
				return
			}
			if err != nil || cfg.Mail != c.want || cfg.ResendAPIKey != c.key || cfg.ResendWebhookSecret != c.hook || cfg.MailAlertTo != c.alert ||
				cfg.SMTPHost != c.host || cfg.SMTPUsername != c.user || cfg.SMTPPassword != c.pass {
				t.Fatalf("cfg %+v err %v, want TS_MAIL %s", cfg, err, c.want)
			}
		})
	}
	t.Setenv("TS_DATABASE_URL", "postgres://x")
	t.Setenv("TS_PUBLIC_URL", "")
	for _, k := range []string{"TS_MAIL", "TS_MAIL_FROM", "TS_RESEND_API_URL", "TS_RESEND_API_KEY", "TS_RESEND_WEBHOOK_SECRET", "TS_MAIL_ALERT_TO", "TS_SMTP_PORT"} {
		t.Setenv(k, "")
	}
	if cfg, err := Load(); err != nil || cfg.MailFrom != "TicketSlinger <tickets@ticketslinger.com>" || cfg.ResendAPIURL != "https://api.resend.com" || cfg.SMTPPort != 587 {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
}

func TestIsLocalHost(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "127.9.9.9": true, "example.com": false, "10.0.0.1": false, "": false} {
		if IsLocalHost(host) != want {
			t.Errorf("IsLocalHost(%q) = %v", host, !want)
		}
	}
}
