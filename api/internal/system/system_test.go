package system_test

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/system"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// Callers lists every package allowed to act as the storefront or the
// system. Adding one is a review decision: say why in the PR.
var callers = map[string]string{
	"github.com/ticketslinger/ticketslinger/api/internal/sales": "buyers' checkouts, webhooks, storefront reads",
}

// switchesRole matches any way SQL can change the current role.
var switchesRole = regexp.MustCompile(`(?i)\b(set\s+(local\s+|session\s+)?role|reset\s+role)\b|set_config\(\s*'role'`)

// TestOnlyListedPackagesUseSystem walks the whole api module: only listed
// packages import system, and nothing but system (Go or SQL, tests included)
// switches roles. Migrations grant roles; they never switch to one.
func TestOnlyListedPackagesUseSystem(t *testing.T) {
	const module = "github.com/ticketslinger/ticketslinger/api/"
	root := filepath.Join("..", "..")
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if d.IsDir() || filepath.Ext(path) != ".go" && filepath.Ext(path) != ".sql" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		pkg := module + filepath.ToSlash(filepath.Dir(rel))
		scanned++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if pkg != module+"internal/system" && switchesRole.Match(src) {
			t.Errorf("%s switches roles; only package system may", rel)
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == module+"internal/system" {
				if _, ok := callers[pkg]; !ok {
					t.Errorf("%s imports system but isn't listed in callers", rel)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files; is the walk rooted at the api module?", scanned)
	}
	if !switchesRole.MatchString("select set_config( 'role', 'ts_system', true)") || !switchesRole.MatchString("RESET ROLE") || !switchesRole.MatchString("set session role x") {
		t.Fatal("switchesRole misses a form of role switch")
	}
}

func TestRolesAreSetOnlyNotInherited(t *testing.T) {
	pool := testdb.New(t)
	var n int
	err := testdb.Owner(t, pool).QueryRow(context.Background(), `select count(*) from pg_auth_members m
		join pg_roles r on r.oid = m.roleid join pg_roles u on u.oid = m.member
		where u.rolname = 'ts_app' and r.rolname in ('ts_public', 'ts_system')
		  and not m.inherit_option and m.set_option
		  and not r.rolcanlogin and not r.rolbypassrls and not r.rolsuper`).Scan(&n)
	if err != nil || n != 2 {
		t.Fatalf("ts_app should be able to SET ROLE to both (NOLOGIN, NOBYPASSRLS, NOSUPERUSER), inheriting neither: %d %v", n, err)
	}
	// Nobody else may become them, and ts_app can't hand them on.
	var others []string
	err = testdb.Owner(t, pool).QueryRow(context.Background(), `select coalesce(array_agg(u.rolname || ' in ' || r.rolname || case when m.admin_option then ' with admin' else '' end), '{}')
		from pg_auth_members m join pg_roles r on r.oid = m.roleid join pg_roles u on u.oid = m.member
		where r.rolname in ('ts_public', 'ts_system') and (u.rolname <> 'ts_app' or m.admin_option)`).Scan(&others)
	if err != nil || len(others) != 0 {
		t.Fatalf("memberships beyond ts_app's SET-only grants: %v %v", others, err)
	}
}

// seedOrganizer inserts, as the owner, an organizer with a live event (one
// visible and one hidden ticket type, a question for everyone and one only
// for the hidden type, a quota and a promo code) and a draft event with one
// visible ticket type.
func seedOrganizer(t *testing.T, pool *pgxpool.Pool, slug string) (org, live, draft uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	owner := testdb.Owner(t, pool)
	if err := owner.QueryRow(ctx, `insert into organizers(name, slug) values ('Org', $1) returning id`, slug).Scan(&org); err != nil {
		t.Fatal(err)
	}
	event := func(slug, status string) (id uuid.UUID) {
		if err := owner.QueryRow(ctx, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at, status)
			values ($1, $2, 'Show', 'UTC', now() + interval '1 day', now() + interval '2 days', $3) returning id`, org, slug, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		_, err := owner.Exec(ctx, `insert into ticket_types(organizer_id, event_id, name, price_cents, position) values ($1, $2, 'General', 1000, 1)`, org, id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	live, draft = event("live-show", "live"), event("draft-show", "draft")
	for _, q := range []string{
		`insert into ticket_types(organizer_id, event_id, name, price_cents, hidden, position) values ($1, $2, 'Crew', 0, true, 2)`,
		`insert into questions(organizer_id, event_id, label, kind, scope, position) values ($1, $2, 'Name?', 'text', 'order', 1)`,
		`with q as (insert into questions(organizer_id, event_id, label, kind, scope, position, applies_to_all)
			values ($1, $2, 'Crew radio channel?', 'text', 'ticket', 2, false) returning id)
		 insert into question_applies_to(organizer_id, event_id, question_id, ticket_type_id)
			select $1, $2, q.id, t.id from q, ticket_types t where t.event_id = $2 and t.name = 'Crew'`,
		`insert into quotas(organizer_id, event_id, name, size) values ($1, $2, 'Room', 100)`,
		`insert into promo_codes(organizer_id, event_id, code, kind, value) values ($1, $2, 'FRIENDS', 'percent', 1000)`,
	} {
		if _, err := owner.Exec(ctx, q, org, live); err != nil {
			t.Fatal(err)
		}
	}
	return org, live, draft
}

func TestPublicSeesOnlyLiveEventsAndVisibleTickets(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	seedOrganizer(t, pool, "acme")
	// An organizer with only a draft event stays invisible.
	if _, err := testdb.Owner(t, pool).Exec(ctx, `with o as (insert into organizers(name, slug) values ('Drafty', 'drafty') returning id)
		insert into events(organizer_id, slug, name, timezone, starts_at, ends_at)
		select id, 'soon', 'Soon', 'UTC', now(), now() + interval '1 hour' from o`); err != nil {
		t.Fatal(err)
	}
	err := system.Public(ctx, pool, func(tx pgx.Tx) error {
		var events, tts, questions, applies int
		if err := tx.QueryRow(ctx, `select (select count(*) from events), (select count(*) from ticket_types),
			(select count(*) from questions), (select count(*) from question_applies_to)`).Scan(&events, &tts, &questions, &applies); err != nil {
			return err
		}
		if events != 1 || tts != 1 || questions != 1 || applies != 0 {
			t.Errorf("public sees %d events, %d ticket types, %d questions, %d question links; want the live one, its visible type, its open question, no links to the hidden type",
				events, tts, questions, applies)
		}
		var slugs []string
		if err := tx.QueryRow(ctx, `select array_agg(slug::text order by slug) from organizers`).Scan(&slugs); err != nil || len(slugs) != 1 || slugs[0] != "acme" {
			t.Errorf("public sees organizers %v (%v), want only acme, which has a live event", slugs, err)
		}
		var members int
		if err := tx.QueryRow(ctx, `select count(*) from memberships`).Scan(&members); err != nil || members != 0 {
			t.Errorf("public sees %d memberships (%v), want 0", members, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Each refusal aborts its transaction, so each gets its own.
	for _, q := range []string{
		`select stripe_account_id from organizers limit 1`,
		`select count(*) from quotas`,
		`select count(*) from promo_codes`,
		`select count(*) from api_keys`,
		`select count(*) from audit_log`,
		`select count(*) from users`,
		`select count(*) from sessions`,
		`insert into events(organizer_id, slug, name, timezone, starts_at, ends_at) select id, 'x-show', 'X', 'UTC', now(), now() + interval '1 hour' from organizers`,
	} {
		err := system.Public(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		var pg *pgconn.PgError
		// A write stops at the read-only transaction before privileges are checked.
		refused := errors.As(err, &pg) && (pg.Code == "42501" || pg.Code == "25006" && strings.HasPrefix(q, "insert"))
		if !refused {
			t.Errorf("public %q: %v, want refused", q, err)
		}
	}
}

func TestSystemIsScopedToOneOrganizer(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	a, _, _ := seedOrganizer(t, pool, "org-a")
	b, _, _ := seedOrganizer(t, pool, "org-b")
	err := system.Run(ctx, pool, a, func(tx pgx.Tx) error {
		var orgs, events, questions, applies int
		if err := tx.QueryRow(ctx, `select (select count(*) from organizers), (select count(*) from events),
			(select count(*) from questions), (select count(*) from question_applies_to)`).Scan(&orgs, &events, &questions, &applies); err != nil {
			return err
		}
		if orgs != 1 || events != 2 || questions != 2 || applies != 1 {
			t.Errorf("system scoped to A sees %d organizers, %d events, %d questions, %d question links; want 1, 2, 2, 1", orgs, events, questions, applies)
		}
		var members int
		if err := tx.QueryRow(ctx, `select count(*) from memberships`).Scan(&members); err != nil || members != 0 {
			t.Errorf("system sees %d memberships (%v), want 0", members, err)
		}
		tag, err := tx.Exec(ctx, `update promo_codes set used_count = used_count + 1 where organizer_id = $1`, b)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("system scoped to A updated %d of B's promo codes", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `update promo_codes set used_count = used_count + 1 where organizer_id = $1`, a)
		if err != nil || tag.RowsAffected() != 1 {
			t.Errorf("system scoped to A counting A's promo use: %d rows, %v", tag.RowsAffected(), err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Run(ctx, pool, uuid.Nil, func(pgx.Tx) error { return nil }); err == nil {
		t.Fatal("system without an organizer must be refused")
	}
	err = system.Run(ctx, pool, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update promo_codes set code = 'OTHER'`)
		return err
	})
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" {
		t.Fatalf("system rewriting a promo code: %v, want 42501", err)
	}
	for _, q := range []string{
		`select count(*) from api_keys`,
		`select count(*) from audit_log`,
		`select count(*) from users`,
		`select count(*) from sessions`,
		`insert into events(organizer_id, slug, name, timezone, starts_at, ends_at) values ((select app_scope()), 'x-show', 'X', 'UTC', now(), now() + interval '1 hour')`,
	} {
		err := system.Run(ctx, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("system %q: %v, want 42501", q, err)
		}
	}
}

// SELECT ... FOR UPDATE needs UPDATE rights, so ts_system has UPDATE (size)
// on quotas; the policy's WITH CHECK (false) keeps every write out.
func TestSystemCanLockButNotResizeQuotas(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	a, _, _ := seedOrganizer(t, pool, "org-a")
	err := system.Run(ctx, pool, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select id from quotas order by id for update`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err == nil && len(ids) != 1 {
			t.Errorf("system locked %d quotas, want 1", len(ids))
		}
		return err
	})
	if err != nil {
		t.Fatalf("lock quotas: %v", err)
	}
	err = system.Run(ctx, pool, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `update quotas set size = 1`)
		return err
	})
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" {
		t.Fatalf("system resizing a quota: %v, want 42501", err)
	}
}
