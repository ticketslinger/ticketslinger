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

// t0 is next year, so an event at t0 hasn't ended when a test puts it on sale.
var t0 = time.Date(time.Now().Year()+1, 3, 1, 19, 0, 0, 0, time.UTC)

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
	// Both have finished Stripe onboarding, so paid tickets can go live.
	if _, err := testdb.Owner(t, pool).Exec(ctx, `update organizers set stripe_account_id = 'acct_' || replace(slug::text, '-', ''), stripe_link_method = 'created', stripe_charges_enabled = true`); err != nil {
		t.Fatal(err)
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
		up, err = catalogue.UpdateEvent(ctx, tx, a, ev.ID, actor, catalogue.EventPatch{Name: &name}, catalogue.PaymentsConnect)
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
		_, err := catalogue.UpdateEvent(ctx, tx, a, ev.ID, audit.Actor{User: user}, catalogue.EventPatch{StartsAt: &later}, catalogue.PaymentsConnect)
		return err
	})
	if want := (catalogue.ErrInvalid{}); !errors.As(err, &want) || want.Code != "bad_times" {
		t.Errorf("patch start past end: %v", err)
	}
	// Going live waits on Task 5's ticket types; ended and cancelled aren't open yet.
	for status, code := range map[string]string{"live": "no_ticket_types", "ended": "status_not_allowed", "cancelled": "status_not_allowed", "bogus": "status_not_allowed"} {
		err := as(t, pool, user, a, func(tx pgx.Tx) error {
			_, err := catalogue.UpdateEvent(ctx, tx, a, ev.ID, audit.Actor{User: user}, catalogue.EventPatch{Status: &status}, catalogue.PaymentsConnect)
			return err
		})
		if want := (catalogue.ErrInvalid{}); !errors.As(err, &want) || want.Code != code {
			t.Errorf("status %s: want %s, got %v", status, code, err)
		}
	}
}

func conflict(err error, code string) bool {
	var c catalogue.ErrConflict
	return errors.As(err, &c) && c.Code == code
}

func TestSlugEditsAndStatusRules(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a, actor := orgs[0], audit.Actor{User: user}
	mk := func(slug string) (ev catalogue.Event) {
		if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in(slug)); return }); err != nil {
			t.Fatal(err)
		}
		return
	}
	patch := func(id uuid.UUID, p catalogue.EventPatch) (ev catalogue.Event, err error) {
		err = as(t, pool, user, a, func(tx pgx.Tx) (e error) {
			ev, e = catalogue.UpdateEvent(ctx, tx, a, id, actor, p, catalogue.PaymentsConnect)
			return
		})
		return
	}
	str := func(s string) *string { return &s }
	one, two := mk("event-one"), mk("event-two")

	if got, err := patch(one.ID, catalogue.EventPatch{Slug: str("renamed")}); err != nil || got.Slug != "renamed" {
		t.Fatalf("draft slug change: %v %+v", err, got)
	}
	if _, err := patch(one.ID, catalogue.EventPatch{Slug: str("event-two")}); !conflict(err, "slug_taken") {
		t.Fatalf("taken slug: %v", err)
	}
	if _, err := patch(one.ID, catalogue.EventPatch{Slug: str("Bad Slug")}); err == nil || !errors.As(err, &catalogue.ErrInvalid{}) {
		t.Fatalf("invalid slug: %v", err)
	}

	// A no-op patch writes nothing: same updated_at, no audit row.
	var n int
	count := func() int {
		_ = as(t, pool, user, a, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select count(*) from audit_log where action = 'event.updated'`).Scan(&n)
		})
		return n
	}
	before := count()
	same := two.Name
	got, err := patch(two.ID, catalogue.EventPatch{Name: &same, StartsAt: &two.StartsAt})
	if err != nil || !got.UpdatedAt.Equal(two.UpdatedAt) || count() != before {
		t.Fatalf("no-op patch: err=%v updated %v vs %v, audit %d vs %d", err, got.UpdatedAt, two.UpdatedAt, count(), before)
	}

	// Past draft the address is locked; past live the status is.
	owner := testdb.Owner(t, pool)
	if _, err := owner.Exec(ctx, `update events set status = 'live' where id = $1`, two.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := patch(two.ID, catalogue.EventPatch{Slug: str("new-address")}); !conflict(err, "slug_locked") {
		t.Fatalf("live slug change: %v", err)
	}
	for _, status := range []string{"ended", "cancelled"} {
		if _, err := owner.Exec(ctx, `update events set status = $2 where id = $1`, two.ID, status); err != nil {
			t.Fatal(err)
		}
		for _, to := range []string{"draft", "live"} {
			_, err := patch(two.ID, catalogue.EventPatch{Status: &to})
			if want := (catalogue.ErrInvalid{}); !errors.As(err, &want) || want.Code != "status_not_allowed" {
				t.Errorf("%s -> %s: %v", status, to, err)
			}
		}
	}
}

func TestCreateAndDeleteAreAudited(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a, actor := orgs[0], audit.Actor{User: user}
	var ev catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in("audited")); return }); err != nil {
		t.Fatal(err)
	}
	if err := as(t, pool, user, a, func(tx pgx.Tx) error { return catalogue.DeleteEvent(ctx, tx, a, ev.ID, actor) }); err != nil {
		t.Fatal(err)
	}
	type row struct {
		actor         *uuid.UUID
		entity        uuid.UUID
		before, after *string
	}
	get := func(action string) (r row) {
		if err := as(t, pool, user, a, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `select actor_user_id, entity_id, before::text, after::text from audit_log where action = $1`, action).
				Scan(&r.actor, &r.entity, &r.before, &r.after)
		}); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return
	}
	c, d := get("event.created"), get("event.deleted")
	if c.actor == nil || *c.actor != user || c.entity != ev.ID || c.before != nil || c.after == nil {
		t.Errorf("created: %+v", c)
	}
	var m map[string]any
	if c.after != nil {
		_ = json.Unmarshal([]byte(*c.after), &m)
	}
	if m["slug"] != "audited" || m["id"] != ev.ID.String() {
		t.Errorf("created after: %v", m)
	}
	if d.actor == nil || *d.actor != user || d.entity != ev.ID || d.before == nil || d.after != nil {
		t.Errorf("deleted: %+v", d)
	}
}

func TestGoLiveNeedsPaymentsForPaidTickets(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	org, actor := orgs[0], audit.Actor{User: user}
	live, draft := "live", "draft"
	stripe := func(ready bool) {
		t.Helper()
		if _, err := testdb.Owner(t, pool).Exec(ctx, `update organizers set stripe_charges_enabled = $2 where id = $1`, org, ready); err != nil {
			t.Fatal(err)
		}
	}
	stripe(false)
	var ev catalogue.Event
	var free catalogue.TicketType
	if err := as(t, pool, user, org, func(tx pgx.Tx) (err error) {
		if ev, err = catalogue.CreateEvent(ctx, tx, org, actor, catalogue.EventInput{Slug: "gala", Name: "Gala", Timezone: "UTC",
			StartsAt: time.Now().Add(24 * time.Hour), EndsAt: time.Now().Add(27 * time.Hour)}); err != nil {
			return err
		}
		if free, err = catalogue.CreateTicketType(ctx, tx, org, ev.ID, actor, catalogue.TicketTypeInput{Name: "Free"}); err != nil {
			return err
		}
		_, err = catalogue.CreateQuota(ctx, tx, org, ev.ID, actor, catalogue.QuotaInput{Name: "Hall", Size: 100, TicketTypeIDs: []uuid.UUID{free.ID}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	mode := catalogue.PaymentsConnect
	status := func(s *string) error {
		return as(t, pool, user, org, func(tx pgx.Tx) error {
			_, err := catalogue.UpdateEvent(ctx, tx, org, ev.ID, actor, catalogue.EventPatch{Status: s}, mode)
			return err
		})
	}
	if err := status(&live); err != nil {
		t.Fatalf("free-only event without Stripe: %v", err)
	}
	if err := status(&draft); err != nil {
		t.Fatal(err)
	}
	if err := as(t, pool, user, org, func(tx pgx.Tx) error {
		paid, err := catalogue.CreateTicketType(ctx, tx, org, ev.ID, actor, catalogue.TicketTypeInput{Name: "General", PriceCents: 2500})
		if err != nil {
			return err
		}
		_, err = catalogue.CreateQuota(ctx, tx, org, ev.ID, actor, catalogue.QuotaInput{Name: "Floor", Size: 50, TicketTypeIDs: []uuid.UUID{paid.ID}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var inv catalogue.ErrInvalid
	if err := status(&live); !errors.As(err, &inv) || inv.Code != "payments_not_ready" || inv.Field != "status" {
		t.Fatalf("paid ticket without Stripe: %v", err)
	}
	mode = catalogue.PaymentsDirect // the server's own Stripe account: nothing to connect
	if err := status(&live); err != nil {
		t.Fatalf("paid ticket without Stripe, direct mode: %v", err)
	}
	if err := status(&draft); err != nil {
		t.Fatal(err)
	}
	mode = catalogue.PaymentsConnect
	stripe(true)
	if err := status(&live); err != nil {
		t.Fatalf("paid ticket with Stripe: %v", err)
	}
}
