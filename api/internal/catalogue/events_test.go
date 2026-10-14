package catalogue_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

var t0 = time.Date(2027, 3, 1, 19, 0, 0, 0, time.UTC)

func in(slug string) catalogue.EventInput {
	return catalogue.EventInput{Slug: slug, Name: "Spring Gala", Timezone: "Europe/London", StartsAt: t0, EndsAt: t0.Add(3 * time.Hour)}
}

func seed(t *testing.T, pool *pgxpool.Pool) (user uuid.UUID, orgs [2]uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `insert into users(email, name) values ('a@example.com', 'T') returning id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	for i, slug := range []string{"org-a", "org-b"} {
		if err := db.WithUser(ctx, pool, user, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select create_organizer('Org', $1)`, slug).Scan(&orgs[i])
		}); err != nil {
			t.Fatal(err)
		}
	}
	return user, orgs
}

// as runs fn as the user inside org.
func as(t *testing.T, pool *pgxpool.Pool, user, org uuid.UUID, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return db.WithOrganizer(context.Background(), pool, user, org, func(tx pgx.Tx, _ string) error { return fn(tx) })
}

func TestEventLifecycle(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	actor := audit.Actor{User: user}
	a, b := orgs[0], orgs[1]

	var ev catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in("spring-gala"))
		return
	}); err != nil {
		t.Fatal(err)
	}
	if ev.Status != "draft" || ev.FeeMode != "pass" || ev.Currency != "USD" || ev.TaxRateBP != 0 || ev.PricesIncludeTax {
		t.Fatalf("defaults: %+v", ev)
	}

	// Same slug: taken in this organizer, fine in another.
	err := as(t, pool, user, a, func(tx pgx.Tx) error {
		_, err := catalogue.CreateEvent(ctx, tx, a, actor, in("spring-gala"))
		return err
	})
	if want := (catalogue.ErrConflict{}); !errors.As(err, &want) || want.Code != "slug_taken" {
		t.Fatalf("duplicate slug: %v", err)
	}
	if err := as(t, pool, user, b, func(tx pgx.Tx) error {
		_, err := catalogue.CreateEvent(ctx, tx, b, actor, in("spring-gala"))
		return err
	}); err != nil {
		t.Fatalf("same slug in another organizer: %v", err)
	}

	// Patching moves updated_at and writes one audit row with before and after.
	name := "Autumn Gala"
	var up catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		up, err = catalogue.UpdateEvent(ctx, tx, a, ev.ID, actor, catalogue.EventPatch{Name: &name})
		return
	}); err != nil {
		t.Fatal(err)
	}
	if up.Name != name || !up.UpdatedAt.After(ev.UpdatedAt) || up.Slug != ev.Slug {
		t.Fatalf("patched: %+v", up)
	}
	var n int
	var before, after []byte
	if err := as(t, pool, user, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select count(*), min(before::text)::bytea, min(after::text)::bytea from audit_log where action = 'event.updated'`).Scan(&n, &before, &after)
	}); err != nil {
		t.Fatal(err)
	}
	var bm, am map[string]any
	_ = json.Unmarshal(before, &bm)
	_ = json.Unmarshal(after, &am)
	if n != 1 || bm["name"] != "Spring Gala" || am["name"] != name {
		t.Fatalf("audit: n=%d before=%v after=%v", n, bm, am)
	}

	// An event in another organizer is invisible, not forbidden.
	err = as(t, pool, user, b, func(tx pgx.Tx) error { _, err := catalogue.GetEvent(ctx, tx, ev.ID); return err })
	if !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("get across organizers: %v", err)
	}

	// Only drafts can be deleted.
	if _, err := testdb.Owner(t, pool).Exec(ctx, `update events set status = 'live' where id = $1`, ev.ID); err != nil {
		t.Fatal(err)
	}
	err = as(t, pool, user, a, func(tx pgx.Tx) error { return catalogue.DeleteEvent(ctx, tx, a, ev.ID, actor) })
	if want := (catalogue.ErrConflict{}); !errors.As(err, &want) || want.Code != "event_not_draft" {
		t.Fatalf("delete live: %v", err)
	}
}

func TestListIsOrderedByStart(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a := orgs[0]
	for i, slug := range []string{"later", "sooner"} {
		e := in(slug)
		e.StartsAt, e.EndsAt = t0.Add(time.Duration(2-i)*24*time.Hour), t0.Add(time.Duration(2-i)*24*time.Hour+time.Hour)
		if err := as(t, pool, user, a, func(tx pgx.Tx) error {
			_, err := catalogue.CreateEvent(ctx, tx, a, audit.Actor{User: user}, e)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	var list []catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { list, err = catalogue.ListEvents(ctx, tx); return }); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Slug != "sooner" {
		t.Fatalf("order: %+v", list)
	}
}

func TestEventValidation(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a := orgs[0]
	long := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'x'
		}
		return string(b)
	}
	cases := []struct {
		code string
		mod  func(*catalogue.EventInput)
	}{
		{"bad_times", func(e *catalogue.EventInput) { e.EndsAt = e.StartsAt }},
		{"bad_timezone", func(e *catalogue.EventInput) { e.Timezone = "Mars/Olympus" }},
		{"bad_timezone", func(e *catalogue.EventInput) { e.Timezone = "Local" }},
		{"bad_timezone", func(e *catalogue.EventInput) { e.Timezone = "" }},
		{"invalid_slug", func(e *catalogue.EventInput) { e.Slug = "Bad Slug" }},
		{"invalid_slug", func(e *catalogue.EventInput) { e.Slug = "ab" }},
		{"invalid_name", func(e *catalogue.EventInput) { e.Name = "  " }},
		{"invalid_name", func(e *catalogue.EventInput) { e.Name = long(161) }},
		{"invalid_description", func(e *catalogue.EventInput) { e.Description = long(20001) }},
		{"invalid_venue", func(e *catalogue.EventInput) { e.VenueName = long(161) }},
		{"invalid_venue", func(e *catalogue.EventInput) { e.VenueAddress = long(501) }},
		{"invalid_tax_rate", func(e *catalogue.EventInput) { e.TaxRateBP = 10001 }},
		{"invalid_tax_rate", func(e *catalogue.EventInput) { e.TaxRateBP = -1 }},
		{"invalid_fee_mode", func(e *catalogue.EventInput) { e.FeeMode = "free" }},
	}
	for _, c := range cases {
		e := in("valid-slug")
		c.mod(&e)
		err := as(t, pool, user, a, func(tx pgx.Tx) error {
			_, err := catalogue.CreateEvent(ctx, tx, a, audit.Actor{User: user}, e)
			return err
		})
		if want := (catalogue.ErrInvalid{}); !errors.As(err, &want) || want.Code != c.code {
			t.Errorf("want %s, got %v", c.code, err)
		}
	}

	// A patch that leaves ends_at before the new start is bad_times too.
	var ev catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		ev, err = catalogue.CreateEvent(ctx, tx, a, audit.Actor{User: user}, in("patch-me"))
		return
	}); err != nil {
		t.Fatal(err)
	}
	later := ev.EndsAt.Add(time.Hour)
	err := as(t, pool, user, a, func(tx pgx.Tx) error {
		_, err := catalogue.UpdateEvent(ctx, tx, a, ev.ID, audit.Actor{User: user}, catalogue.EventPatch{StartsAt: &later})
		return err
	})
	if want := (catalogue.ErrInvalid{}); !errors.As(err, &want) || want.Code != "bad_times" {
		t.Errorf("patch start past end: %v", err)
	}
	// Going live waits on Task 5's ticket types; ended and cancelled aren't open yet.
	for status, code := range map[string]string{"live": "no_ticket_types", "ended": "status_not_allowed", "cancelled": "status_not_allowed", "bogus": "status_not_allowed"} {
		err := as(t, pool, user, a, func(tx pgx.Tx) error {
			_, err := catalogue.UpdateEvent(ctx, tx, a, ev.ID, audit.Actor{User: user}, catalogue.EventPatch{Status: &status})
			return err
		})
		if want := (catalogue.ErrInvalid{}); !errors.As(err, &want) || want.Code != code {
			t.Errorf("status %s: want %s, got %v", status, code, err)
		}
	}
}
