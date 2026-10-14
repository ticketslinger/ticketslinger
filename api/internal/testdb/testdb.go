// Package testdb gives each test its own migrated database.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
)

// New creates a throwaway database owned by ts_owner, migrates it, and returns
// a pool connected as ts_app. The database is dropped when the test ends.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ownerURL, appURL, skip, err := requireURLs(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Skip("TEST_DATABASE_URL_OWNER/APP not set; run `make dev-db`")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "t_" + hex.EncodeToString(b)

	admin, err := db.Open(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" OWNER ts_owner"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close()
	})

	if err := db.Migrate(ctx, withDB(t, ownerURL, name)); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, withDB(t, appURL, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Owner opens a ts_owner pool on the same database as app (a pool from New),
// for fixtures the app role may not write itself. It bypasses RLS.
func Owner(t *testing.T, app *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ownerURL, _, _, err := requireURLs(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(context.Background(), withDB(t, ownerURL, app.Config().ConnConfig.Database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// requireURLs reads the test database URLs. When they are missing, it asks
// for a skip locally but returns an error under CI so tests cannot pass vacuously.
func requireURLs(getenv func(string) string) (owner, app string, skip bool, err error) {
	owner, app = getenv("TEST_DATABASE_URL_OWNER"), getenv("TEST_DATABASE_URL_APP")
	if owner != "" && app != "" {
		return owner, app, false, nil
	}
	if getenv("CI") != "" {
		return "", "", false, errors.New("TEST_DATABASE_URL_OWNER/APP must be set when CI is set; refusing to skip database tests")
	}
	return "", "", true, nil
}

func withDB(t *testing.T, raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
