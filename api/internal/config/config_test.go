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
