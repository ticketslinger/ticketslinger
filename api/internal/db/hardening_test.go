package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// A read key's transaction is read-only in the database, so even raw SQL that
// RLS would let through can't write.
func TestReadKeyScopeIsReadOnly(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	_, oa := seedOrg(t, pool, "a@example.com", "org-a")
	read := seedKey(t, pool, oa, "read", false)
	write := seedKey(t, pool, oa, "write", false)
	insert := func(tx pgx.Tx, _ string) error {
		_, err := tx.Exec(ctx, `insert into audit_log(organizer_id, action, entity_type, entity_id) values ($1, 'x', 'organizer', $1)`, oa)
		return err
	}
	err := db.WithPrincipal(ctx, pool, db.Principal{KeyID: read, KeyOrg: oa, KeyScope: "read"}, oa, insert)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "25006" {
		t.Fatalf("write in a read-key scope: %v, want 25006 read_only_sql_transaction", err)
	}
	if err := db.WithPrincipal(ctx, pool, db.Principal{KeyID: write, KeyOrg: oa, KeyScope: "write"}, oa, insert); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// Every tenant table and every SECURITY DEFINER function, checked from the
// catalog, so a new migration can't quietly skip the hardening.
func TestCatalogHardening(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	owner := testdb.Owner(t, pool)
	rows, err := owner.Query(ctx, `select c.relname, c.relrowsecurity,
			exists (select 1 from pg_policy p where p.polrelid = c.oid and pg_get_expr(p.polqual, p.polrelid) like '%app_organizer()%')
		from pg_class c join pg_attribute a on a.attrelid = c.oid and a.attname = 'organizer_id' and not a.attisdropped
		where c.relnamespace = 'public'::regnamespace and c.relkind in ('r', 'p') order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var name string
		var rls, policy bool
		if err := rows.Scan(&name, &rls, &policy); err != nil {
			t.Fatal(err)
		}
		n++
		if !rls || !policy {
			t.Errorf("%s: row security %v, app_organizer() policy %v", name, rls, policy)
		}
	}
	if rows.Err() != nil || n < 10 {
		t.Fatalf("tenant tables: %d %v", n, rows.Err())
	}

	rows, err = owner.Query(ctx, `select p.oid::regprocedure::text, coalesce(p.proconfig, '{}'),
			exists (select 1 from aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) x where x.grantee = 0 and x.privilege_type = 'EXECUTE')
		from pg_proc p where p.pronamespace = 'public'::regnamespace and p.prosecdef order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	n = 0
	for rows.Next() {
		var fn string
		var config []string
		var public bool
		if err := rows.Scan(&fn, &config, &public); err != nil {
			t.Fatal(err)
		}
		n++
		pinned := false
		for _, c := range config {
			if strings.HasPrefix(c, "search_path=") && strings.HasSuffix(c, "pg_temp") {
				pinned = true
			}
		}
		if !pinned || public {
			t.Errorf("%s: search_path pinned to end in pg_temp %v (%v), PUBLIC can execute %v", fn, pinned, config, public)
		}
	}
	if rows.Err() != nil || n == 0 {
		t.Fatalf("definer functions: %d %v", n, rows.Err())
	}
}

// The storefront and system roles are the only exception to "tenant policies
// call app_organizer()", every system policy is pinned to app_scope(), and
// neither role can read identity, key or audit tables. organizers is a tenant
// table keyed by id rather than organizer_id, so it is named here.
func TestSaleRolesStayNarrow(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	owner := testdb.Owner(t, pool)
	rows, err := owner.Query(ctx, `select c.relname || '.' || p.polname,
			array(select case when r = 0 then 'PUBLIC' else r::regrole::text end from unnest(p.polroles) r),
			coalesce(pg_get_expr(p.polqual, p.polrelid), '') || ' ' || coalesce(pg_get_expr(p.polwithcheck, p.polrelid), '')
		from pg_policy p join pg_class c on c.oid = p.polrelid
		where c.relnamespace = 'public'::regnamespace
		  and (c.relname = 'organizers' or exists (select 1 from pg_attribute a
		       where a.attrelid = c.oid and a.attname = 'organizer_id' and not a.attisdropped))
		order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	exceptions, organizers := 0, 0
	for rows.Next() {
		var name, expr string
		var roles []string
		if err := rows.Scan(&name, &roles, &expr); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(name, "organizers.") {
			organizers++
		}
		if strings.Contains(expr, "app_organizer()") {
			continue
		}
		exceptions++
		if len(roles) == 0 {
			t.Errorf("%s skips app_organizer() and applies to no role list", name)
		}
		for _, r := range roles {
			switch {
			case r != "ts_public" && r != "ts_system":
				t.Errorf("%s skips app_organizer() but applies to %s; only ts_public and ts_system may", name, r)
			case r == "ts_system" && !strings.Contains(expr, "app_scope()"):
				t.Errorf("%s is a ts_system policy that doesn't call app_scope(): %s", name, expr)
			}
		}
	}
	if rows.Err() != nil || exceptions == 0 || organizers == 0 {
		t.Fatalf("policies outside app_organizer(): %d, on organizers: %d, %v", exceptions, organizers, rows.Err())
	}

	// memberships stays readable: org_reads (00002) subqueries it with the
	// caller's rights, and member_reads shows these roles no rows. The one
	// right either has here: ts_system appends its own audit rows (00023).
	for _, role := range []string{"ts_public", "ts_system"} {
		for _, table := range []string{"users", "sessions", "passkeys", "webauthn_challenges", "recovery_codes", "email_tokens", "api_keys", "audit_log", "idempotency_keys", "login_failures"} {
			rights := "SELECT, INSERT, UPDATE, REFERENCES"
			if role == "ts_system" && table == "audit_log" {
				rights = "SELECT, UPDATE, REFERENCES"
			}
			var any bool
			err := owner.QueryRow(ctx, `select has_any_column_privilege($1, $2, $3)
				or has_table_privilege($1, $2, 'DELETE, TRUNCATE, TRIGGER')`, role, table, rights).Scan(&any)
			if err != nil || any {
				t.Errorf("%s has rights on %s: %v %v", role, table, any, err)
			}
		}
	}
}
