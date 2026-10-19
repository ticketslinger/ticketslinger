package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
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
	_, org := seedOrg(t, pool, "a@example.com", "org-a")
	// A live event: the storefront role may read it, plain ts_app still may not.
	if _, err := testdb.Owner(t, pool).Exec(context.Background(), `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
		values ($1, 'live-show', 'Show', 'UTC', now(), now() + interval '1 hour', 'live')`, org); err != nil {
		t.Fatal(err)
	}
	seedSale(t, pool, org)
	for _, table := range append([]string{"organizers", "memberships", "audit_log", "events"}, salesTables...) {
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
	seedSale(t, pool, oa)
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('app.user_id', $1, true), set_config('app.organizer_id', $2, true)`, ub.String(), oa.String()); err != nil {
			return err
		}
		qs := []string{
			`select count(*) from organizers where id = $1`,
			`select count(*) from memberships where organizer_id = $1`,
			`select count(*) from audit_log where organizer_id = $1`,
		}
		for _, table := range salesTables {
			qs = append(qs, `select count(*) from `+table+` where organizer_id = $1`)
		}
		for _, q := range qs {
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
	// The app role holds no UPDATE or DELETE privilege at all: refused
	// outright (42501), not merely filtered to zero rows by a policy. Nor can
	// a member write a row as if the system had done it.
	for _, q := range []string{`update audit_log set action = 'x'`, `delete from audit_log`,
		`insert into audit_log(organizer_id, actor_system, action, entity_type, entity_id) values ((select app_organizer()), true, 'test', 'order', gen_random_uuid())`} {
		err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%q: got %v, want SQLSTATE 42501 insufficient_privilege", q, err)
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

// seedKey inserts an API key for org as the schema owner and returns its id.
func seedKey(t *testing.T, pool *pgxpool.Pool, org uuid.UUID, scope string, revoked bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := testdb.Owner(t, pool).QueryRow(context.Background(),
		`insert into api_keys(organizer_id, name, key_hash, prefix, scope, revoked_at)
		 values ($1, 'k', sha256(gen_random_uuid()::text::bytea), 'abcdefgh', $2, case when $3 then now() end) returning id`,
		org, scope, revoked).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// countTenant counts org's rows in every tenant table from inside tx.
func countTenant(ctx context.Context, t *testing.T, tx pgx.Tx, org uuid.UUID) map[string]int {
	t.Helper()
	out := map[string]int{}
	for table, q := range map[string]string{
		"organizers":  `select count(*) from organizers where id = $1`,
		"memberships": `select count(*) from memberships where organizer_id = $1`,
		"audit_log":   `select count(*) from audit_log where organizer_id = $1`,
		"api_keys":    `select count(*) from api_keys where organizer_id = $1`,
	} {
		var n int
		if err := tx.QueryRow(ctx, q, org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func TestRLSKeySeesItsOwnOrganizer(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	_, oa := seedOrg(t, pool, "a@example.com", "org-a")
	_, ob := seedOrg(t, pool, "b@example.com", "org-b")
	seedKey(t, pool, ob, "write", false)
	key := seedKey(t, pool, oa, "read", false)
	if _, err := testdb.Owner(t, pool).Exec(ctx, `insert into audit_log(organizer_id, action, entity_type, entity_id)
		select id, 'test', 'organizer', id from organizers`); err != nil {
		t.Fatal(err)
	}
	p := db.Principal{KeyID: key, KeyOrg: oa, KeyScope: "read"}
	err := db.WithPrincipal(ctx, pool, p, oa, func(tx pgx.Tx, role string) error {
		if role != "key:read" {
			t.Errorf("role=%q, want key:read", role)
		}
		got := countTenant(ctx, t, tx, oa)
		for table, n := range got {
			if n != 1 {
				t.Errorf("%s: key saw %d rows of its own organizer, want 1", table, n)
			}
		}
		var all int
		if err := tx.QueryRow(ctx, `select count(*) from api_keys`).Scan(&all); err != nil {
			return err
		}
		if all != 1 {
			t.Errorf("key saw %d api_keys, want only its organizer's 1", all)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWithPrincipalRejectsKeyForAnotherOrganizer(t *testing.T) {
	pool := testdb.New(t)
	_, oa := seedOrg(t, pool, "a@example.com", "org-a")
	_, ob := seedOrg(t, pool, "b@example.com", "org-b")
	key := seedKey(t, pool, oa, "write", false)
	err := db.WithPrincipal(context.Background(), pool, db.Principal{KeyID: key, KeyOrg: oa, KeyScope: "write"}, ob,
		func(pgx.Tx, string) error { t.Error("fn must not run for another organizer"); return nil })
	if !errors.Is(err, db.ErrNotMember) {
		t.Errorf("err=%v, want ErrNotMember", err)
	}
	err = db.WithPrincipal(context.Background(), pool, db.Principal{UserID: uuid.New(), KeyID: key, KeyOrg: oa, KeyScope: "write"}, oa,
		func(pgx.Tx, string) error { t.Error("a user and a key together must not run"); return nil })
	if err == nil {
		t.Error("user and key in one principal: want an error")
	}
}

// Bypass Go: set the key GUCs by hand. A key with another tenant's
// organizer_id, a revoked key, and a key alongside a user all get no scope.
func TestRLSForgedKeyScopesSeeNothing(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	_, ob := seedOrg(t, pool, "b@example.com", "org-b")
	seedKey(t, pool, ob, "write", false)
	keyA := seedKey(t, pool, oa, "write", false)
	revokedB := seedKey(t, pool, ob, "write", true)
	cases := []struct {
		name           string
		user, key, org string
		target         uuid.UUID
	}{
		{"key with another tenant's organizer", "", keyA.String(), ob.String(), ob},
		{"revoked key", "", revokedB.String(), ob.String(), ob},
		{"user and key together", ua.String(), keyA.String(), oa.String(), oa},
	}
	for _, c := range cases {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select set_config('app.user_id', $1, true), set_config('app.api_key_id', $2, true), set_config('app.organizer_id', $3, true)`,
				c.user, c.key, c.org); err != nil {
				return err
			}
			var scoped *uuid.UUID
			if err := tx.QueryRow(ctx, `select app_organizer()`).Scan(&scoped); err != nil {
				return err
			}
			if scoped != nil {
				t.Errorf("%s: app_organizer() = %v, want NULL", c.name, scoped)
			}
			got := countTenant(ctx, t, tx, c.target)
			for _, table := range []string{"audit_log", "api_keys"} {
				if got[table] != 0 {
					t.Errorf("%s: saw %d %s rows", c.name, got[table], table)
				}
			}
			if c.user == "" && (got["organizers"] != 0 || got["memberships"] != 0) {
				t.Errorf("%s: saw %v", c.name, got)
			}
			tag, err := tx.Exec(ctx, `update organizers set name = 'pwned' where id = $1`, c.target)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("%s: updated %d organizers", c.name, tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

func TestAuditRowWrittenByKeyNamesTheKey(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	_, oa := seedOrg(t, pool, "a@example.com", "org-a")
	key := seedKey(t, pool, oa, "write", false)
	p := db.Principal{KeyID: key, KeyOrg: oa, KeyScope: "write"}
	err := db.WithPrincipal(ctx, pool, p, oa, func(tx pgx.Tx, _ string) error {
		if err := audit.Write(ctx, tx, audit.Entry{Organizer: oa, Actor: audit.Actor{Key: key}, Action: "test.done", EntityType: "organizer", EntityID: oa}); err != nil {
			return err
		}
		var user, k *uuid.UUID
		if err := tx.QueryRow(ctx, `select actor_user_id, actor_api_key_id from audit_log where action = 'test.done'`).Scan(&user, &k); err != nil {
			return err
		}
		if user != nil || k == nil || *k != key {
			t.Errorf("actor_user_id=%v actor_api_key_id=%v, want NULL and %v", user, k, key)
		}
		// Both actors at once: Go refuses, and so does the database.
		if err := audit.Write(ctx, tx, audit.Entry{Organizer: oa, Actor: audit.Actor{User: uuid.New(), Key: key}, Action: "x", EntityType: "organizer", EntityID: oa}); err == nil {
			t.Error("audit.Write with two actors: want an error")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = testdb.Owner(t, pool).Exec(ctx, `insert into audit_log(organizer_id, actor_user_id, actor_api_key_id, action, entity_type, entity_id)
		select $1, user_id, $2, 'x', 'organizer', $1 from memberships where organizer_id = $1`, oa, key)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("two actors in the database: err=%v, want check violation", err)
	}
}

// Keys never manage keys, even with raw SQL inside their own scope: a write
// key may read its organizer's keys but not mint, upgrade, un-revoke or delete
// one. (A read key can't write anything: TestReadKeyScopeIsReadOnly.) A user
// scope still can.
func TestKeyScopeCannotManageKeys(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	key := seedKey(t, pool, oa, "read", false)
	revoked := seedKey(t, pool, oa, "write", true)
	writer := seedKey(t, pool, oa, "write", false)
	p := db.Principal{KeyID: writer, KeyOrg: oa, KeyScope: "write"}
	inKeyScope := func(q string, args ...any) (int64, error) {
		var n int64
		err := db.WithPrincipal(ctx, pool, p, oa, func(tx pgx.Tx, _ string) error {
			tag, err := tx.Exec(ctx, q, args...)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	_, err := inKeyScope(`insert into api_keys(organizer_id, name, key_hash, prefix, scope)
		values ($1, 'evil', sha256('x'::bytea), 'abcdefgh', 'write')`, oa)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("key inserting a key: err=%v, want 42501", err)
	}
	for name, q := range map[string]string{
		"upgrade a key":   `update api_keys set scope = 'write' where id = '` + key.String() + `'`,
		"un-revoke a key": `update api_keys set revoked_at = null where id = '` + revoked.String() + `'`,
		"delete a key":    `delete from api_keys where id = '` + revoked.String() + `'`,
	} {
		n, err := inKeyScope(q)
		if err != nil || n != 0 {
			t.Errorf("%s: rows=%d err=%v, want 0 rows", name, n, err)
		}
	}
	var scope string
	var rev *time.Time
	if err := testdb.Owner(t, pool).QueryRow(ctx, `select (select scope from api_keys where id = $1), (select revoked_at from api_keys where id = $2)`, key, revoked).Scan(&scope, &rev); err != nil || scope != "read" || rev == nil {
		t.Fatalf("after the attempts: scope=%q revoked_at=%v err=%v", scope, rev, err)
	}
	// Control: the key can still read, and a user scope can still manage.
	var seen int
	if err := db.WithPrincipal(ctx, pool, p, oa, func(tx pgx.Tx, _ string) error {
		return tx.QueryRow(ctx, `select count(*) from api_keys`).Scan(&seen)
	}); err != nil || seen != 3 {
		t.Fatalf("key reading keys: n=%d err=%v, want 3", seen, err)
	}
	err = db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
		if _, err := tx.Exec(ctx, `insert into api_keys(organizer_id, name, key_hash, prefix, scope)
			values ($1, 'ok', sha256('y'::bytea), 'abcdefgh', 'read')`, oa); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `update api_keys set revoked_at = now() where id = $1`, key)
		if err == nil && tag.RowsAffected() != 1 {
			t.Errorf("user revoking: rows=%d, want 1", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Fatalf("user managing keys: %v", err)
	}
}

// The member directory shows an organizer's members to its own members and to
// nobody else: not a forged scope, not a missing user, not an API key alone.
func TestMemberDirectoryIsTenantScoped(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	ub, ob := seedOrg(t, pool, "b@example.com", "org-b")
	var keyID uuid.UUID
	if err := testdb.Owner(t, pool).QueryRow(ctx,
		`insert into api_keys(organizer_id, name, key_hash, prefix, scope) values ($1, 'K', '\x01', 'abcdefgh', 'read') returning id`, oa).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	count := func(user, key, org uuid.UUID) (n int) {
		t.Helper()
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select set_config('app.user_id', $1, true), set_config('app.api_key_id', $2, true), set_config('app.organizer_id', $3, true)`,
				nilEmpty(user), nilEmpty(key), nilEmpty(org)); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `select count(*) from app_member_directory()`).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(ua, uuid.Nil, oa); n != 1 {
		t.Errorf("member of A sees %d rows of A, want 1", n)
	}
	if n := count(ub, uuid.Nil, ob); n != 1 {
		t.Errorf("member of B sees %d rows of B, want 1", n)
	}
	if n := count(ub, uuid.Nil, oa); n != 0 {
		t.Errorf("user of B forging scope on A sees %d rows, want 0", n)
	}
	if n := count(uuid.Nil, uuid.Nil, oa); n != 0 {
		t.Errorf("no user sees %d rows, want 0", n)
	}
	if n := count(uuid.Nil, keyID, oa); n != 0 {
		t.Errorf("API key alone sees %d rows, want 0", n)
	}

	// Only owners and admins may read colleagues' emails.
	var uc uuid.UUID
	root := testdb.Owner(t, pool)
	if err := root.QueryRow(ctx, `insert into users(email, name) values ('c@example.com', 'C') returning id`).Scan(&uc); err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]int{"checkin": 0, "admin": 2} {
		if _, err := root.Exec(ctx, `delete from memberships where user_id = $1`, uc); err != nil {
			t.Fatal(err)
		}
		if _, err := root.Exec(ctx, `insert into memberships(organizer_id, user_id, role) values ($1, $2, $3)`, oa, uc, role); err != nil {
			t.Fatal(err)
		}
		if n := count(uc, uuid.Nil, oa); n != want {
			t.Errorf("%s sees %d rows, want %d", role, n, want)
		}
	}
}

func nilEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// The definer function is pinned like its siblings and closed to PUBLIC.
func TestMemberDirectoryIsHardened(t *testing.T) {
	pool := testdb.New(t)
	var cfg []string
	var public, app bool
	err := pool.QueryRow(context.Background(), `select proconfig,
		has_function_privilege('public', 'app_member_directory()', 'execute'),
		has_function_privilege('ts_app', 'app_member_directory()', 'execute')
		from pg_proc where proname = 'app_member_directory'`).Scan(&cfg, &public, &app)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg) != 1 || cfg[0] != "search_path=pg_catalog, public, pg_temp" {
		t.Errorf("proconfig = %v", cfg)
	}
	if public || !app {
		t.Errorf("execute: public=%v ts_app=%v, want false/true", public, app)
	}
}

// The tenant tables 00017 adds for sales. stripe_events has no organizer.
var salesTables = []string{"checkouts", "holds", "orders", "tickets", "answers", "payments", "refunds"}

// seedSale writes one row of each sales table for org through the owner
// pool, as the system role would: a live event, a held cart, a paid order
// with one answered ticket, its payment and a refund.
func seedSale(t *testing.T, app *pgxpool.Pool, org uuid.UUID) {
	t.Helper()
	_, err := testdb.Owner(t, app).Exec(context.Background(), `with
		e as (insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
			values ($1, 'sold-show', 'Show', 'UTC', now(), now() + interval '1 hour', 'live') returning id),
		tt as (insert into ticket_types(organizer_id, event_id, name, price_cents, position) select $1, id, 'General', 2500, 1 from e returning id, event_id),
		q as (insert into questions(organizer_id, event_id, label, kind, scope, position) select $1, id, 'Diet', 'text', 'order', 1 from e returning id),
		c as (insert into checkouts(organizer_id, event_id, token_hash, expires_at)
			select $1, id, sha256('tok'), now() + interval '15 minutes' from e returning id, event_id),
		h as (insert into holds(organizer_id, event_id, checkout_id, ticket_type_id, quantity) select $1, c.event_id, c.id, tt.id, 1 from c, tt),
		o as (insert into orders(organizer_id, event_id, code, email, status, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents,
				fee_mode, quote, details)
			select $1, id, 'TS-ABCDE', 'b@example.com', 'paid', 2500, 0, 0, 100, 2600, 'pass', '{}', '{}' from e returning id, event_id),
		k as (insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, charge_cents, secret)
			select $1, o.event_id, o.id, tt.id, 2500, 0, 0, 100, 2500, decode(md5(random()::text), 'hex') from o, tt returning id),
		a as (insert into answers(organizer_id, event_id, order_id, question_id, value) select $1, o.event_id, o.id, q.id, 'None' from o, q),
		p as (insert into payments(organizer_id, order_id, provider, stripe_account_id, payment_intent_id, client_secret, amount_cents, application_fee_cents)
			select $1, id, 'test', 'acct_1', 'pi_1', 'pi_1_secret', 2600, 100 from o returning id)
		insert into refunds(organizer_id, payment_id, amount_cents, fee_refunded, status, reason)
			select $1, id, 2600, true, 'succeeded', 'organizer' from p`, org)
	if err != nil {
		t.Fatal(err)
	}
}

// Members read their sales but can't write them: only the system role does.
func TestRLSAppCannotWriteSales(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	seedSale(t, pool, oa)
	err := db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from tickets`).Scan(&n); err != nil || n != 1 {
			t.Errorf("own tickets: %d %v, want 1", n, err)
		}
		_, err := tx.Exec(ctx, `insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, charge_cents, secret)
			select organizer_id, event_id, order_id, ticket_type_id, 0, 0, 0, 0, 0, decode(md5(random()::text), 'hex') from tickets`)
		return err
	})
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" {
		t.Fatalf("ts_app insert into tickets: %v, want 42501", err)
	}
}

// A ticket, an answer or an order's promo code can't point at a row of
// another event, even one of the same organizer.
func TestSalesRowsStayInTheirEvent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	_, org := seedOrg(t, pool, "a@example.com", "org-a")
	seedSale(t, pool, org)
	owner := testdb.Owner(t, pool)
	if _, err := owner.Exec(ctx, `with e as (insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
			values ($1, 'other-show', 'Other', 'UTC', now(), now() + interval '1 hour', 'live') returning id),
		t as (insert into ticket_types(organizer_id, event_id, name, price_cents, position) select $1, id, 'Other', 100, 1 from e)
		insert into questions(organizer_id, event_id, label, kind, scope, position) select $1, id, 'Other', 'text', 'order', 1 from e`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `insert into promo_codes(organizer_id, event_id, code, kind, value)
		select $1, id, 'SOLD', 'percent', 1000 from events where slug = 'sold-show'`, org); err != nil {
		t.Fatal(err)
	}
	other := `(select id from events where slug = 'other-show')`
	for name, q := range map[string]string{
		"ticket on another event's order": `insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, charge_cents, secret)
			select $1, ` + other + `, (select id from orders), (select id from ticket_types where name = 'Other'), 0, 0, 0, 0, 0, decode(md5(random()::text), 'hex')`,
		"answer on another event's order": `insert into answers(organizer_id, event_id, order_id, question_id, value)
			select $1, ` + other + `, (select id from orders), (select id from questions where label = 'Other'), 'x'`,
		"order with another event's code": `insert into orders(organizer_id, event_id, code, email, subtotal_cents, discount_cents, tax_cents, fee_cents, total_cents,
				fee_mode, quote, details, promo_code_id)
			select $1, ` + other + `, 'TS-ZZZZZ', 'b@example.com', 0, 0, 0, 0, 0, 'pass', '{}', '{}', (select id from promo_codes)`,
	} {
		_, err := owner.Exec(ctx, q, org)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23503" {
			t.Errorf("%s: %v, want 23503", name, err)
		}
	}
}

// Members see their payments but never the client secret, which only the
// buyer's checkout needs.
func TestAppCannotReadClientSecret(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	seedSale(t, pool, oa)
	read := func(cols string) error {
		return db.WithOrganizer(ctx, pool, ua, oa, func(tx pgx.Tx, _ string) error {
			_, err := tx.Exec(ctx, `select `+cols+` from payments`)
			return err
		})
	}
	if err := read("id, order_id, payment_intent_id, amount_cents, application_fee_cents, status"); err != nil {
		t.Fatalf("payment columns: %v", err)
	}
	var pg *pgconn.PgError
	if err := read("client_secret"); !errors.As(err, &pg) || pg.Code != "42501" {
		t.Fatalf("client_secret: %v, want 42501", err)
	}
}

// quota_used runs with the caller's rights and only the app and system roles
// may call it.
func TestQuotaUsedIsNotPublic(t *testing.T) {
	pool := testdb.New(t)
	var public, app, system bool
	err := testdb.Owner(t, pool).QueryRow(context.Background(), `select
		exists (select 1 from pg_proc p, aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) x
		         where p.oid = 'quota_used(uuid, uuid)'::regprocedure and x.grantee = 0),
		has_function_privilege('ts_app', 'quota_used(uuid, uuid)', 'EXECUTE'),
		has_function_privilege('ts_system', 'quota_used(uuid, uuid)', 'EXECUTE')`).Scan(&public, &app, &system)
	if err != nil || public || !app || !system {
		t.Fatalf("PUBLIC %v, ts_app %v, ts_system %v, %v", public, app, system, err)
	}
}

// add_member and remove_member check ownership themselves, whatever the Go
// handler does first: an admin gets 'forbidden', and no role but admin or
// checkin gets in at all.
func TestTeamFunctionsAreOwnersOnly(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	ua, oa := seedOrg(t, pool, "a@example.com", "org-a")
	ub, _ := seedOrg(t, pool, "b@example.com", "org-b")
	owner := testdb.Owner(t, pool)
	if _, err := owner.Exec(ctx, `update users set email_verified_at = now();
		insert into users(email, name, email_verified_at) values ('c@example.com', 'C', now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `insert into memberships(organizer_id, user_id, role) values ($1, $2, 'admin')`, oa, ub); err != nil {
		t.Fatal(err)
	}
	call := func(user uuid.UUID, q string, args ...any) (string, error) {
		var r string
		err := db.WithOrganizer(ctx, pool, user, oa, func(tx pgx.Tx, _ string) error {
			return tx.QueryRow(ctx, q, args...).Scan(&r)
		})
		return r, err
	}
	for q, args := range map[string][]any{
		`select add_member('c@example.com', 'checkin')`: nil,
		`select remove_member($1)`:                      {ua},
	} {
		if r, err := call(ub, q, args...); err != nil || r != "forbidden" {
			t.Errorf("admin %s: %q %v, want forbidden", q, r, err)
		}
	}
	var pgErr *pgconn.PgError
	if _, err := call(ua, `select add_member('c@example.com', 'owner')`); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("role owner: %v, want check_violation", err)
	}
	if r, err := call(ua, `select remove_member($1)`, ua); err != nil || r != "cannot_remove" {
		t.Fatalf("owner removing themself: %q %v", r, err)
	}
	var n int
	if err := owner.QueryRow(ctx, `select count(*) from memberships where organizer_id = $1`, oa).Scan(&n); err != nil || n != 2 {
		t.Fatalf("memberships: %d %v, want 2", n, err)
	}
}
