package config

import "testing"

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
