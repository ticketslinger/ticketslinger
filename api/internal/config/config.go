// Package config reads runtime settings from TS_* environment variables.
package config

import (
	"errors"
	"os"
)

type Config struct {
	DatabaseURL        string // ts_app
	MigrateDatabaseURL string // ts_owner; only needed by `ticketslinger migrate`
	HTTPAddr           string
	PublicURL          string
	DashboardOrigin    string
	WebAuthnRPID       string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:        os.Getenv("TS_DATABASE_URL"),
		MigrateDatabaseURL: os.Getenv("TS_MIGRATE_DATABASE_URL"),
		HTTPAddr:           env("TS_HTTP_ADDR", "127.0.0.1:3000"),
		PublicURL:          env("TS_PUBLIC_URL", "http://localhost:3000"),
		DashboardOrigin:    env("TS_DASHBOARD_ORIGIN", "http://localhost:5173"),
		WebAuthnRPID:       env("TS_WEBAUTHN_RPID", "localhost"),
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
