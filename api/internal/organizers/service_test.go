package organizers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func newUser(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `insert into users(email, name) values ($1, 'T') returning id`, email).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreateListResolveAudit(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	svc := organizers.New(pool)
	ann, bob := newUser(t, pool, "ann@example.com"), newUser(t, pool, "bob@example.com")

	o, err := svc.Create(ctx, ann, "Acme", "acme")
	if err != nil || o.Role != "owner" || o.ID == uuid.Nil {
		t.Fatalf("create: %+v %v", o, err)
	}
	if _, err := svc.Create(ctx, bob, "Other", "acme"); !errors.Is(err, organizers.ErrSlugTaken) {
		t.Fatalf("duplicate slug: %v", err)
	}
	if l, err := svc.ListForUser(ctx, ann); err != nil || len(l) != 1 || l[0].Slug != "acme" || l[0].Role != "owner" {
		t.Fatalf("list: %+v %v", l, err)
	}
	if l, err := svc.ListForUser(ctx, bob); err != nil || len(l) != 0 {
		t.Fatalf("bob list: %+v %v", l, err)
	}
	if got, err := svc.Resolve(ctx, ann, "acme"); err != nil || got.ID != o.ID {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	if _, err := svc.Resolve(ctx, bob, "acme"); !errors.Is(err, organizers.ErrNotFound) {
		t.Fatalf("resolve as stranger: %v", err)
	}

	var n int
	err = db.WithOrganizer(ctx, pool, ann, o.ID, func(tx pgx.Tx, _ string) error {
		return tx.QueryRow(ctx, `select count(*) from audit_log where action = 'organizer.created' and entity_id = $1 and actor_user_id = $2`, o.ID, ann).Scan(&n)
	})
	if err != nil || n != 1 {
		t.Fatalf("audit rows = %d, %v", n, err)
	}
}

func TestDBRefusesBadSlugAndName(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	ann := newUser(t, pool, "ann@example.com")
	for _, c := range [][2]string{{"A", "ACME"}, {"A", "ab"}, {"A", "a--b"}, {"A", "orders"}, {"A", "healthz"}, {"", "acme"}, {strings.Repeat("n", 121), "acme"}} {
		err := db.WithUser(ctx, pool, ann, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select create_organizer($1, $2)`, c[0], c[1])
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23514" {
			t.Errorf("%q/%q: want check violation, got %v", c[0], c[1], err)
		}
	}
}
