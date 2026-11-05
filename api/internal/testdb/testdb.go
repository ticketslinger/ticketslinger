// Package testdb gives each test its own migrated database.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
)

// staleAfter is how old a test database must be before a later run drops it.
// Cleanup never runs when a test binary is killed, so without the sweep those
// databases pile up (7,000+ and 77 GB on one dev volume).
const staleAfter = time.Hour

var sweepOnce sync.Once

// stale reports whether name is a test database created before cutoff.
// Names are t_<unix seconds, hex>_<random hex>; anything else is left alone.
func stale(name string, cutoff time.Time) bool {
	ts, _, ok := strings.Cut(strings.TrimPrefix(name, "t_"), "_")
	if !ok || !strings.HasPrefix(name, "t_") {
		return false
	}
	sec, err := strconv.ParseInt(ts, 16, 64)
	return err == nil && time.Unix(sec, 0).Before(cutoff)
}

// sweep drops test databases that earlier, killed runs left behind.
func sweep(ctx context.Context, admin *pgxpool.Pool) {
	rows, err := admin.Query(ctx, "SELECT datname FROM pg_database WHERE datname LIKE 't\\_%'")
	if err != nil {
		return
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil && stale(n, time.Now().Add(-staleAfter)) {
			names = append(names, n)
		}
	}
	rows.Close()
	for _, n := range names {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+n+" WITH (FORCE)")
	}
}

// dropDB drops a test database. pgxpool.Close returns before the server
// backends exit, and ts_owner may not terminate ts_app's backends, so FORCE
// alone fails on a straggler; wait for them to go first.
func dropDB(t *testing.T, admin *pgxpool.Pool, name string) {
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		var n int
		if admin.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = $1", name).Scan(&n) != nil || n == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
		t.Logf("testdb: drop %s: %v (a later run sweeps it)", name, err)
	}
}

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
	name := fmt.Sprintf("t_%x_%s", time.Now().Unix(), hex.EncodeToString(b))

	admin, err := db.Open(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	sweepOnce.Do(func() { sweep(ctx, admin) })
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" OWNER ts_owner"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropDB(t, admin, name)
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
