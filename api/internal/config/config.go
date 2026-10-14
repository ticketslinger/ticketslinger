// Package config reads runtime settings from TS_* environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL        string // ts_app
	MigrateDatabaseURL string // ts_owner; only needed by `ticketslinger migrate`
	HTTPAddr           string
	PublicURL          string
	DashboardOrigin    string
	WebAuthnRPID       string
	// SecretKey encrypts secrets at rest (TS_SECRET_KEY: base64 of 32 bytes).
	// Nil when unset; `serve` refuses to start without it.
	SecretKey []byte
	// TrustedProxies may set the client address with X-Real-IP.
	TrustedProxies []netip.Prefix
	// Platform fee per paid ticket: basis points of the pre-tax price plus a flat amount.
	PlatformFeeBP    int   // TS_PLATFORM_FEE_BP, 0-10000, default 200
	PlatformFeeCents int64 // TS_PLATFORM_FEE_CENTS, 0-10000, default 50
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
