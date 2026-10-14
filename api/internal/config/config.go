// Package config reads runtime settings from TS_* environment variables.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

type Config struct {
	DatabaseURL        string // ts_app
	MigrateDatabaseURL string // ts_owner; only needed by `ticketslinger migrate`
	HTTPAddr           string
	PublicURL          string
	DashboardOrigin    string
	WebAuthnRPID       string
	// TrustedProxies may set the client address with X-Real-IP.
	TrustedProxies []netip.Prefix
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
