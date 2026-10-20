package db_test

import (
	"context"
	"errors"
	"slices"
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

// ownerOnly tables carry an organizer_id but only the schema owner's
// definer functions read or write them.
var ownerOnly = map[string]bool{"organizer_suspensions": true, "staff_actions": true}

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
		if !rls || !policy && !ownerOnly[name] {
			t.Errorf("%s: row security %v, app_organizer() policy %v", name, rls, policy)
		}
	}
	if rows.Err() != nil || n < 10 {
		t.Fatalf("tenant tables: %d %v", n, rows.Err())
	}

	// The takedown tables have an organizer_id but no tenant reads them:
	// row security on, no policy at all, and no right for any app role.
	for name := range ownerOnly {
		var policies int
		var rls bool
		var granted []string
		if err := owner.QueryRow(ctx, `select c.relrowsecurity, (select count(*) from pg_policy p where p.polrelid = c.oid),
				array(select r from unnest(array['ts_app', 'ts_public', 'ts_system']) r
				       where has_table_privilege(r, c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
				          or has_any_column_privilege(r, c.oid, 'SELECT, INSERT, UPDATE, REFERENCES'))
			from pg_class c where c.oid = to_regclass($1)`, name).Scan(&rls, &policies, &granted); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !rls || policies != 0 || len(granted) != 0 {
			t.Errorf("%s: row security %v, %d policies, rights for %v; want on, none, none", name, rls, policies, granted)
		}
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

	// Takedown: ts_app checks and acts, ts_system checks and runs the
	// refund, and the storefront role runs none of them.
	for fn, want := range map[string][2]bool{
		"organizer_suspended(uuid)":           {true, true},
		"staff_act(uuid,uuid,text,uuid,text)": {true, false},
		"claim_staff_refund(uuid)":            {false, true},
		"finish_staff_refund(uuid,jsonb)":     {false, true},
	} {
		var definer, app, public, system bool
		err := owner.QueryRow(ctx, `select p.prosecdef, has_function_privilege('ts_app', p.oid, 'EXECUTE'),
				has_function_privilege('ts_public', p.oid, 'EXECUTE'), has_function_privilege('ts_system', p.oid, 'EXECUTE')
			from pg_proc p where p.oid = to_regprocedure($1)`, fn).Scan(&definer, &app, &public, &system)
		if err != nil || !definer || app != want[0] || public || system != want[1] {
			t.Errorf("%s: definer %v, ts_app %v, ts_public %v, ts_system %v, %v", fn, definer, app, public, system, err)
		}
	}

	// Staff mode's functions are all definers (so the loop above covers
	// them), ts_app runs all but the scope function, which only
	// app_organizer() calls, and neither sale role runs any.
	for _, fn := range []string{"app_staff_organizer()", "app_is_staff()", "start_staff_session(uuid,citext,text,inet,text)",
		"end_staff_session(uuid,text,text)", "log_staff_access(uuid,text,text)", "close_expired_staff_sessions(integer)",
		"staff_search_organizers(text)", "app_staff_directory()", "staff_session_actions(uuid)", "staff_session_slug(uuid)"} {
		var definer, app, public, system bool
		err := owner.QueryRow(ctx, `select p.prosecdef, has_function_privilege('ts_app', p.oid, 'EXECUTE'),
				has_function_privilege('ts_public', p.oid, 'EXECUTE'), has_function_privilege('ts_system', p.oid, 'EXECUTE')
			from pg_proc p where p.oid = to_regprocedure($1)`, fn).Scan(&definer, &app, &public, &system)
		if err != nil || !definer || app != (fn != "app_staff_organizer()") || public || system {
			t.Errorf("%s: definer %v, ts_app %v, ts_public %v, ts_system %v, %v", fn, definer, app, public, system, err)
		}
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

	// ts_public runs one query (resolveLive: a live event by its slugs), so
	// it reads exactly those columns, plus memberships for org_reads below,
	// has its two policies, and runs two SECURITY DEFINER functions:
	// app_organizer(), which the TO PUBLIC tenant policies call (NULL here),
	// and queue_order_email_by_link, the buyer's "email me again", which
	// needs the order's code and link token.
	var cols, pols, funcs []string
	if err := owner.QueryRow(ctx, `select
		array(select c.relname || '.' || a.attname from pg_class c join pg_attribute a on a.attrelid = c.oid
		       where c.relnamespace = 'public'::regnamespace and c.relkind = 'r' and c.relname <> 'memberships'
		         and a.attnum > 0 and not a.attisdropped and has_column_privilege('ts_public', c.oid, a.attnum, 'SELECT')
		       order by 1),
		array(select c.relname || '.' || p.polname from pg_policy p join pg_class c on c.oid = p.polrelid
		       where 'ts_public'::regrole = any(p.polroles) order by 1),
		array(select p.oid::regprocedure::text from pg_proc p
		       where p.pronamespace = 'public'::regnamespace and p.prosecdef and has_function_privilege('ts_public', p.oid, 'EXECUTE')
		       order by 1)`).Scan(&cols, &pols, &funcs); err != nil {
		t.Fatal(err)
	}
	if want := []string{"events.id", "events.organizer_id", "events.slug", "organizers.id", "organizers.slug"}; !slices.Equal(cols, want) {
		t.Errorf("ts_public reads %v, want only %v", cols, want)
	}
	if want := []string{"events.events_public", "organizers.organizers_public"}; !slices.Equal(pols, want) {
		t.Errorf("ts_public policies %v, want only %v", pols, want)
	}
	if want := []string{"app_organizer()", "queue_order_email_by_link(text,bytea)"}; !slices.Equal(funcs, want) {
		t.Errorf("ts_public runs SECURITY DEFINER functions %v, want only %v", funcs, want)
	}
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
		var tables []string
		if err := owner.QueryRow(ctx, `select array(select c.relname::text from pg_class c
			where c.relnamespace = 'public'::regnamespace and c.relkind = 'r'
			  and (has_table_privilege('ts_public', c.oid, $1) or $1 in ('INSERT', 'UPDATE', 'REFERENCES') and has_any_column_privilege('ts_public', c.oid, $1)))`,
			priv).Scan(&tables); err != nil || len(tables) != 0 {
			t.Errorf("ts_public has %s on %v (%v)", priv, tables, err)
		}
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

// The order page goes through order_by_link, which needs the code and the
// token together. Nothing answers "does this code exist?" on its own.
func TestNoOrderCodeOracle(t *testing.T) {
	pool := testdb.New(t)
	var scope, link *string
	if err := testdb.Owner(t, pool).QueryRow(context.Background(),
		`select to_regprocedure('order_scope(text)')::text, to_regprocedure('order_by_link(text, bytea)')::text`).Scan(&scope, &link); err != nil {
		t.Fatal(err)
	}
	if scope != nil || link == nil {
		t.Fatalf("order_scope exists: %v, order_by_link exists: %v; want only order_by_link", scope != nil, link != nil)
	}
}

// The refund job finds its rows by payload->>'refund_id'; the index keeps
// that from scanning every job.
func TestJobsRefundIndexExists(t *testing.T) {
	pool := testdb.New(t)
	var def string
	if err := testdb.Owner(t, pool).QueryRow(context.Background(),
		`select indexdef from pg_indexes where schemaname = 'public' and indexname = 'jobs_refund_id_idx'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "WHERE (kind = 'refund'::text)") || !strings.Contains(def, "refund_id") {
		t.Fatalf("jobs_refund_id_idx = %s", def)
	}
}
