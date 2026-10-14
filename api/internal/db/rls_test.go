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
	for _, table := range []string{"organizers", "memberships", "audit_log", "events"} {
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
	// The app role holds no UPDATE or DELETE privilege at all: refused
	// outright (42501), not merely filtered to zero rows by a policy.
	for _, q := range []string{`update audit_log set action = 'x'`, `delete from audit_log`} {
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
