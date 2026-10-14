package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func seedOrg(t *testing.T, pool *pgxpool.Pool, email, slug string) (user, org uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `insert into users(email, name) values ($1, 'T') returning id`, email).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select create_organizer($1, 'Org', $2)`, user, slug).Scan(&org); err != nil {
		t.Fatal(err)
	}
	return user, org
}

func TestRLSUnscopedSeesNothing(t *testing.T) {
	pool := testdb.New(t)
	seedOrg(t, pool, "a@example.com", "org-a")
	var n int
	if err := pool.QueryRow(context.Background(), `select count(*) from organizers`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unscoped query saw %d organizers, want 0", n)
	}
}

func TestRLSUserSeesOnlyOwnOrganizers(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, _ := seedOrg(t, pool, "a@example.com", "org-a")
	seedOrg(t, pool, "b@example.com", "org-b")
	var slugs []string
	err := db.WithUser(ctx, pool, ua, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select slug from organizers`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			slugs = append(slugs, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(slugs) != 1 || slugs[0] != "org-a" {
		t.Fatalf("user A saw %v, want [org-a]", slugs)
	}
}

func TestRLSOrganizerScopeBlocksOtherTenantAudit(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	_, ob := seedOrg(t, pool, "b@example.com", "org-b")
	// Scoped to org A, writing an audit row for org B must hit the RLS check.
	err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into audit_log(organizer_id, actor_user_id, action, entity_type, entity_id) values ($1, $2, 'test', 'organizer', $1)`, ob, ua)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("err=%v, want row-level security violation (42501)", err)
	}
	// Control: the same insert for the scoped organizer succeeds.
	err = db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into audit_log(organizer_id, actor_user_id, action, entity_type, entity_id) values ($1, $2, 'test', 'organizer', $1)`, oa, ua)
		return err
	})
	if err != nil {
		t.Fatalf("own-organizer audit insert: %v", err)
	}
}

func TestOnlyOwnerCanBeCreatedByFunction(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	var role string
	err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select role from memberships where user_id = $1`, ua).Scan(&role)
	})
	if err != nil || role != "owner" {
		t.Fatalf("role=%q err=%v, want owner", role, err)
	}
}
