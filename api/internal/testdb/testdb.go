// Package testdb gives each test its own migrated database.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	ownerURL, appURL := os.Getenv("TEST_DATABASE_URL_OWNER"), os.Getenv("TEST_DATABASE_URL_APP")
	if ownerURL == "" || appURL == "" {
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

func withDB(t *testing.T, raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
