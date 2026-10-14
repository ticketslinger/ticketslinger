package db_test

import (
	"context"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func TestMigratedDatabaseConnectsAsApp(t *testing.T) {
	pool := testdb.New(t)
	var user string
	var bypass bool
	err := pool.QueryRow(context.Background(),
		`select current_user, rolbypassrls from pg_roles where rolname = current_user`).Scan(&user, &bypass)
	if err != nil {
		t.Fatal(err)
	}
	if user != "ts_app" || bypass {
		t.Fatalf("connected as %s bypass=%v, want ts_app without bypass", user, bypass)
	}
}
