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
	if c.HTTPAddr != "127.0.0.1:3000" || c.WebAuthnRPID != "localhost" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}
