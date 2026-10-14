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
	err := db.WithUser(ctx, pool, user, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select create_organizer('Org', $1)`, slug).Scan(&org)
	})
	if err != nil {
		t.Fatal(err)
	}
	return user, org
}

func TestRLSUnscopedSeesNothing(t *testing.T) {
	pool := testdb.New(t)
	seedOrg(t, pool, "a@example.com", "org-a")
	for _, table := range []string{"organizers", "memberships", "audit_log"} {
		var n int
		if err := pool.QueryRow(context.Background(), `select count(*) from `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("unscoped query saw %d rows in %s, want 0", n, table)
		}
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
	err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
		_, err := tx.Exec(ctx, `insert into audit_log(organizer_id, actor_user_id, action, entity_type, entity_id) values ($1, $2, 'test', 'organizer', $1)`, ob, ua)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("err=%v, want row-level security violation (42501)", err)
	}
	// Control: the same insert for the scoped organizer succeeds.
	err = db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
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
	var got string
	err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, r string) error {
		got = r
		return tx.QueryRow(ctx, `select role from memberships where user_id = $1`, ua).Scan(&role)
	})
	if err != nil || role != "owner" || got != "owner" {
		t.Fatalf("role=%q err=%v, want owner", role, err)
	}
}

func TestWithOrganizerRejectsNonMember(t *testing.T) {
	pool := testdb.New(t)
	_, oa := seedOrg(t, pool, "a@example.com", "org-a")
	ub, _ := seedOrg(t, pool, "b@example.com", "org-b")
	err := db.WithOrganizer(context.Background(), pool, ub, oa, func(pgx.Tx, string) error {
		t.Fatal("fn must not run for a non-member")
		return nil
	})
	if !errors.Is(err, db.ErrNotMember) {
		t.Fatalf("err=%v, want ErrNotMember", err)
	}
}

// Bypass the Go check: set the GUCs by hand for a non-member. The database
// must still show nothing of the other organizer.
func TestRLSForgedOrganizerScopeSeesNothing(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	ub, _ := seedOrg(t, pool, "b@example.com", "org-b")
	if err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
		_, err := tx.Exec(ctx, `insert into audit_log(organizer_id, actor_user_id, action, entity_type, entity_id) values ($1, $2, 'test', 'organizer', $1)`, oa, ua)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('app.user_id', $1, true), set_config('app.organizer_id', $2, true)`, ub.String(), oa.String()); err != nil {
			return err
		}
		for _, q := range []string{
			`select count(*) from organizers where id = $1`,
			`select count(*) from memberships where organizer_id = $1`,
			`select count(*) from audit_log where organizer_id = $1`,
		} {
			var n int
			if err := tx.QueryRow(ctx, q, oa).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("%s: saw %d rows of org A as non-member", q, n)
			}
		}
		tag, err := tx.Exec(ctx, `update organizers set name = 'pwned' where id = $1`, oa)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("non-member updated %d rows of org A", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
		_, err := tx.Exec(ctx, `insert into audit_log(organizer_id, actor_user_id, action, entity_type, entity_id) values ($1, $2, 'test', 'organizer', $1)`, oa, ua)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`update audit_log set action = 'x'`, `delete from audit_log`} {
		err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
			tag, err := tx.Exec(ctx, q)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("%q affected %d rows", q, tag.RowsAffected())
			}
			return err
		})
		var pgErr *pgconn.PgError
		if err != nil && (!errors.As(err, &pgErr) || pgErr.Code != "42501") {
			t.Fatalf("%q: unexpected error %v", q, err)
		}
	}
	var n int
	if err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
		return tx.QueryRow(ctx, `select count(*) from audit_log where action = 'test'`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("audit rows=%d err=%v, want 1 untouched", n, err)
	}
}

func TestCreateOrganizerNeedsUserScope(t *testing.T) {
	pool := testdb.New(t)
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `select create_organizer('Org', 'org-x')`).Scan(&id)
	if err == nil {
		t.Fatal("create_organizer without a user scope must fail")
	}
}

// The reviewer's attack: shadow organizers with a temp table whose id default is
// the victim's, then call create_organizer. The attacker must not end up with
// any membership in the victim organizer.
func TestCreateOrganizerResistsTempTableShadowing(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	uv, victim := seedOrg(t, pool, "v@example.com", "org-victim")
	ux, _ := seedOrg(t, pool, "x@example.com", "org-attacker")
	_ = uv
	// Result is irrelevant (it may error, e.g. temp creation is revoked); the outcome is not.
	_ = db.WithUser(ctx, pool, ux, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `create temp table organizers(id uuid default '`+victim.String()+`', name text, slug citext, created_at timestamptz default now())`); err != nil {
			t.Logf("temp table refused: %v", err)
			return err
		}
		t.Error("ts_app must not be able to create temp tables")
		_, err := tx.Exec(ctx, `select create_organizer('Evil', 'evil-org')`)
		return err
	})
	var n int
	err := db.WithUser(ctx, pool, ux, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select count(*) from memberships where organizer_id = $1 and user_id = $2`, victim, ux).Scan(&n)
	})
	if err != nil || n != 0 {
		t.Fatalf("attacker memberships on victim=%d err=%v, want 0", n, err)
	}
}
