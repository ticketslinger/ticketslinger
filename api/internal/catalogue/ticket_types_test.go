package catalogue_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func tt(name string) catalogue.TicketTypeInput {
	return catalogue.TicketTypeInput{Name: name, PriceCents: 2500}
}

func invalidCode(err error, code string) bool {
	var i catalogue.ErrInvalid
	return errors.As(err, &i) && i.Code == code
}

func TestTicketTypeCRUDAndOrder(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a, b, actor := orgs[0], orgs[1], audit.Actor{User: user}
	var ev catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in("gala")); return }); err != nil {
		t.Fatal(err)
	}
	var made []catalogue.TicketType
	for _, n := range []string{"General", "VIP", "Student"} {
		if err := as(t, pool, user, a, func(tx pgx.Tx) error {
			x, err := catalogue.CreateTicketType(ctx, tx, a, ev.ID, actor, tt(n))
			made = append(made, x)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	g := made[0]
	if g.MinPerOrder != 1 || g.MaxPerOrder != 10 || g.Hidden || g.TaxRateBP != nil || g.Position != 1 || made[2].Position != 3 {
		t.Fatalf("defaults: %+v", g)
	}

	// Position then created_at: swap positions by hand, then ties fall to creation order.
	owner := testdb.Owner(t, pool)
	if _, err := owner.Exec(ctx, `update ticket_types set position = 1 where id = $1`, made[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `update ticket_types set position = 0 where id = $1`, made[2].ID); err != nil {
		t.Fatal(err)
	}
	var list []catalogue.TicketType
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { list, err = catalogue.ListTicketTypes(ctx, tx, ev.ID); return }); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Name != "Student" || list[1].Name != "General" || list[2].Name != "VIP" {
		t.Fatalf("order: %+v", list)
	}

	// Patch merges, moves updated_at, audits once; a no-op does neither.
	price, name := int64(3000), g.Name
	var up catalogue.TicketType
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		up, err = catalogue.UpdateTicketType(ctx, tx, a, ev.ID, g.ID, actor, catalogue.TicketTypePatch{PriceCents: &price})
		return
	}); err != nil {
		t.Fatal(err)
	}
	if up.PriceCents != 3000 || up.Name != "General" || !up.UpdatedAt.After(g.UpdatedAt) {
		t.Fatalf("patched: %+v", up)
	}
	var noop catalogue.TicketType
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		noop, err = catalogue.UpdateTicketType(ctx, tx, a, ev.ID, g.ID, actor, catalogue.TicketTypePatch{Name: &name, PriceCents: &price})
		return
	}); err != nil || !noop.UpdatedAt.Equal(up.UpdatedAt) {
		t.Fatalf("no-op: %v %v vs %v", err, noop.UpdatedAt, up.UpdatedAt)
	}
	var n int
	_ = as(t, pool, user, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select count(*) from audit_log where action = 'ticket_type.updated'`).Scan(&n)
	})
	if n != 1 {
		t.Fatalf("updated audits: %d", n)
	}

	// Delete audits; the audit rows for create/delete carry before/after.
	if err := as(t, pool, user, a, func(tx pgx.Tx) error { return catalogue.DeleteTicketType(ctx, tx, a, ev.ID, made[1].ID, actor) }); err != nil {
		t.Fatal(err)
	}
	var created, deleted, delBefore, delAfter, crBefore, crAfter int
	_ = as(t, pool, user, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select
			count(*) filter (where action = 'ticket_type.created'), count(*) filter (where action = 'ticket_type.deleted'),
			count(*) filter (where action = 'ticket_type.deleted' and before is not null), count(*) filter (where action = 'ticket_type.deleted' and after is not null),
			count(*) filter (where action = 'ticket_type.created' and before is not null), count(*) filter (where action = 'ticket_type.created' and after is not null)
			from audit_log`).Scan(&created, &deleted, &delBefore, &delAfter, &crBefore, &crAfter)
	})
	if created != 3 || deleted != 1 || delBefore != 1 || delAfter != 0 || crBefore != 0 || crAfter != 3 {
		t.Fatalf("audits: %d %d %d %d %d %d", created, deleted, delBefore, delAfter, crBefore, crAfter)
	}

	// Another tenant sees none of it, and can't add to it.
	err := as(t, pool, user, b, func(tx pgx.Tx) error { _, err := catalogue.GetTicketType(ctx, tx, ev.ID, g.ID); return err })
	if !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("get across organizers: %v", err)
	}
	err = as(t, pool, user, b, func(tx pgx.Tx) error { _, err := catalogue.ListTicketTypes(ctx, tx, ev.ID); return err })
	if !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("list across organizers: %v", err)
	}
	err = as(t, pool, user, b, func(tx pgx.Tx) error {
		_, err := catalogue.CreateTicketType(ctx, tx, b, ev.ID, actor, tt("Sneak"))
		return err
	})
	if !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("create under another tenant's event: %v", err)
	}
	err = as(t, pool, user, b, func(tx pgx.Tx) error {
		_, err := catalogue.UpdateTicketType(ctx, tx, b, ev.ID, g.ID, actor, catalogue.TicketTypePatch{Name: &name})
		return err
	})
	if !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("patch across organizers: %v", err)
	}
	// A real ticket type under the wrong event of the same tenant is missing too.
	var other catalogue.Event
	_ = as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		other, err = catalogue.CreateEvent(ctx, tx, a, actor, in("other"))
		return
	})
	err = as(t, pool, user, a, func(tx pgx.Tx) error { _, err := catalogue.GetTicketType(ctx, tx, other.ID, g.ID); return err })
	if !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("wrong event: %v", err)
	}

	// Deleting the event takes its ticket types with it.
	if err := as(t, pool, user, a, func(tx pgx.Tx) error { return catalogue.DeleteEvent(ctx, tx, a, ev.ID, actor) }); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `select count(*) from ticket_types where event_id = $1`, ev.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cascade: %d %v", n, err)
	}
}

func TestTicketTypeValidation(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a, actor := orgs[0], audit.Actor{User: user}
	var ev catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in("gala")); return }); err != nil {
		t.Fatal(err)
	}
	s := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	tax := 10001
	cases := []struct {
		code string
		mod  func(*catalogue.TicketTypeInput)
	}{
		{"bad_price", func(i *catalogue.TicketTypeInput) { i.PriceCents = -1 }},
		{"bad_price", func(i *catalogue.TicketTypeInput) { i.PriceCents = 10000001 }},
		{"bad_limits", func(i *catalogue.TicketTypeInput) { i.MinPerOrder, i.MaxPerOrder = ip(5), ip(4) }},
		{"bad_limits", func(i *catalogue.TicketTypeInput) { i.MaxPerOrder = ip(101) }},
		{"bad_limits", func(i *catalogue.TicketTypeInput) { i.MinPerOrder = ip(-2) }},
		{"bad_sales_window", func(i *catalogue.TicketTypeInput) { e := s.Add(-time.Hour); i.SalesStart, i.SalesEnd = &s, &e }},
		{"bad_sales_window", func(i *catalogue.TicketTypeInput) { i.SalesStart, i.SalesEnd = &s, &s }},
		{"bad_limits", func(i *catalogue.TicketTypeInput) { i.MinPerOrder = ip(0) }},
		{"bad_limits", func(i *catalogue.TicketTypeInput) { i.MaxPerOrder = ip(0) }},
		{"invalid_name", func(i *catalogue.TicketTypeInput) { i.Name = "" }},
		{"invalid_description", func(i *catalogue.TicketTypeInput) { i.Description = longStr(2001) }},
		{"invalid_tax_rate", func(i *catalogue.TicketTypeInput) { i.TaxRateBP = &tax }},
	}
	for _, c := range cases {
		x := tt("Valid")
		c.mod(&x)
		err := as(t, pool, user, a, func(tx pgx.Tx) error { _, err := catalogue.CreateTicketType(ctx, tx, a, ev.ID, actor, x); return err })
		if !invalidCode(err, c.code) {
			t.Errorf("want %s, got %v", c.code, err)
		}
	}

	// The merged row is what gets checked: a patch that pushes min past the stored max.
	var made catalogue.TicketType
	_ = as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		made, err = catalogue.CreateTicketType(ctx, tx, a, ev.ID, actor, tt("Valid"))
		return
	})
	min := 11
	err := as(t, pool, user, a, func(tx pgx.Tx) error {
		_, err := catalogue.UpdateTicketType(ctx, tx, a, ev.ID, made.ID, actor, catalogue.TicketTypePatch{MinPerOrder: &min})
		return err
	})
	if !invalidCode(err, "bad_limits") {
		t.Errorf("patch min past max: %v", err)
	}
}

func longStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func TestEventCannotGoLiveIncomplete(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a, actor := orgs[0], audit.Actor{User: user}
	var ev catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in("gala")); return }); err != nil {
		t.Fatal(err)
	}
	live := "live"
	goLive := func() (e catalogue.Event, err error) {
		err = as(t, pool, user, a, func(tx pgx.Tx) (x error) {
			e, x = catalogue.UpdateEvent(ctx, tx, a, ev.ID, actor, catalogue.EventPatch{Status: &live})
			return
		})
		return
	}
	if _, err := goLive(); !invalidCode(err, "no_ticket_types") {
		t.Fatalf("no ticket types: %v", err)
	}
	var made catalogue.TicketType
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		made, err = catalogue.CreateTicketType(ctx, tx, a, ev.ID, actor, tt("General"))
		return
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := goLive(); !invalidCode(err, "unquotaed_ticket_type") {
		t.Fatalf("no quota: %v", err)
	}
	if err := as(t, pool, user, a, func(tx pgx.Tx) error {
		_, err := catalogue.CreateQuota(ctx, tx, a, ev.ID, actor, catalogue.QuotaInput{Name: "Hall", Size: 50, TicketTypeIDs: []uuid.UUID{made.ID}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if e, err := goLive(); err != nil || e.Status != "live" {
		t.Fatalf("with a ticket type: %v %+v", err, e)
	}
	// Live with one ticket type: it can't be removed, but a second one frees the first.
	del := func(id uuid.UUID) error {
		return as(t, pool, user, a, func(tx pgx.Tx) error { return catalogue.DeleteTicketType(ctx, tx, a, ev.ID, id, actor) })
	}
	if err := del(made.ID); !conflict(err, "last_ticket_type_live") {
		t.Fatalf("delete last on live: %v", err)
	}
	var second catalogue.TicketType
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		second, err = catalogue.CreateTicketType(ctx, tx, a, ev.ID, actor, tt("VIP"))
		return
	}); err != nil {
		t.Fatal(err)
	}
	if err := del(made.ID); err != nil {
		t.Fatalf("delete one of two on live: %v", err)
	}
	if err := del(second.ID); !conflict(err, "last_ticket_type_live") {
		t.Fatalf("delete last again: %v", err)
	}
	// Back to draft, and the last one can go.
	draft := "draft"
	if err := as(t, pool, user, a, func(tx pgx.Tx) error {
		_, err := catalogue.UpdateEvent(ctx, tx, a, ev.ID, actor, catalogue.EventPatch{Status: &draft})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := del(second.ID); err != nil {
		t.Fatalf("delete last on draft: %v", err)
	}
}

func ip(n int) *int { return &n }

func TestPatchNullClearsAndAbsentKeeps(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a, actor := orgs[0], audit.Actor{User: user}
	var ev catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in("gala")); return }); err != nil {
		t.Fatal(err)
	}
	s, e, yes := t0, t0.Add(time.Hour), true
	var made catalogue.TicketType
	x := tt("General")
	x.TaxRateBP, x.PricesIncludeTax, x.SalesStart, x.SalesEnd = ip(825), &yes, &s, &e
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
		made, err = catalogue.CreateTicketType(ctx, tx, a, ev.ID, actor, x)
		return
	}); err != nil {
		t.Fatal(err)
	}
	patch := func(p catalogue.TicketTypePatch) (got catalogue.TicketType, err error) {
		err = as(t, pool, user, a, func(tx pgx.Tx) (e error) {
			got, e = catalogue.UpdateTicketType(ctx, tx, a, ev.ID, made.ID, actor, p)
			return
		})
		return
	}
	null := func() (n nullable.Nullable[int]) { n.SetNull(); return }
	name := "Renamed"

	// Absent leaves all four alone.
	got, err := patch(catalogue.TicketTypePatch{Name: &name})
	if err != nil || got.TaxRateBP == nil || got.PricesIncludeTax == nil || got.SalesStart == nil || got.SalesEnd == nil {
		t.Fatalf("absent: %v %+v", err, got)
	}
	// Null clears each, one at a time.
	if got, err = patch(catalogue.TicketTypePatch{TaxRateBP: null()}); err != nil || got.TaxRateBP != nil || got.PricesIncludeTax == nil {
		t.Fatalf("clear tax: %v %+v", err, got)
	}
	var nb nullable.Nullable[bool]
	nb.SetNull()
	if got, err = patch(catalogue.TicketTypePatch{PricesIncludeTax: nb}); err != nil || got.PricesIncludeTax != nil {
		t.Fatalf("clear includes: %v %+v", err, got)
	}
	var nt nullable.Nullable[time.Time]
	nt.SetNull()
	// Clearing the start while the end stays is a valid window.
	if got, err = patch(catalogue.TicketTypePatch{SalesStart: nt}); err != nil || got.SalesStart != nil || got.SalesEnd == nil {
		t.Fatalf("clear start: %v %+v", err, got)
	}
	if got, err = patch(catalogue.TicketTypePatch{SalesEnd: nt}); err != nil || got.SalesEnd != nil {
		t.Fatalf("clear end: %v %+v", err, got)
	}
	// Setting a value works, and the merged window is what's validated.
	var v nullable.Nullable[time.Time]
	v.Set(e)
	if _, err = patch(catalogue.TicketTypePatch{SalesEnd: v}); err != nil {
		t.Fatal(err)
	}
	v.Set(e.Add(time.Hour))
	if _, err = patch(catalogue.TicketTypePatch{SalesStart: v}); !invalidCode(err, "bad_sales_window") {
		t.Fatalf("start past stored end: %v", err)
	}
}

// Deleting the last ticket type races with going live and with moving back
// to draft. However they interleave, the event can't end up live with none.
func TestLiveEventNeverEndsWithoutTicketTypes(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a, actor := orgs[0], audit.Actor{User: user}
	live, draft := "live", "draft"
	for round := 0; round < 15; round++ {
		slug := "race-" + string(rune('a'+round))
		var ev catalogue.Event
		var tk catalogue.TicketType
		if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) {
			if ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in(slug)); err != nil {
				return
			}
			if tk, err = catalogue.CreateTicketType(ctx, tx, a, ev.ID, actor, tt("General")); err != nil {
				return
			}
			_, err = catalogue.CreateQuota(ctx, tx, a, ev.ID, actor, catalogue.QuotaInput{Name: "Hall", Size: 5, TicketTypeIDs: []uuid.UUID{tk.ID}})
			return
		}); err != nil {
			t.Fatal(err)
		}
		// Even rounds race delete against go-live from draft; odd rounds start live
		// and race delete against live -> draft -> live.
		if round%2 == 1 {
			if err := as(t, pool, user, a, func(tx pgx.Tx) error {
				_, err := catalogue.UpdateEvent(ctx, tx, a, ev.ID, actor, catalogue.EventPatch{Status: &live})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = as(t, pool, user, a, func(tx pgx.Tx) error { return catalogue.DeleteTicketType(ctx, tx, a, ev.ID, tk.ID, actor) })
		}()
		go func() {
			defer wg.Done()
			steps := []*string{&live}
			if round%2 == 1 {
				steps = []*string{&draft, &live}
			}
			for _, st := range steps {
				_ = as(t, pool, user, a, func(tx pgx.Tx) error {
					_, err := catalogue.UpdateEvent(ctx, tx, a, ev.ID, actor, catalogue.EventPatch{Status: st})
					return err
				})
			}
		}()
		wg.Wait()
		var status string
		var n int
		if err := testdb.Owner(t, pool).QueryRow(ctx, `select e.status, (select count(*) from ticket_types where event_id = e.id) from events e where e.id = $1`, ev.ID).Scan(&status, &n); err != nil {
			t.Fatal(err)
		}
		if status == "live" && n == 0 {
			t.Fatalf("round %d: live with no ticket types", round)
		}
	}
}

func TestConcurrentCreatesGetDistinctPositions(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	a, actor := orgs[0], audit.Actor{User: user}
	var ev catalogue.Event
	if err := as(t, pool, user, a, func(tx pgx.Tx) (err error) { ev, err = catalogue.CreateEvent(ctx, tx, a, actor, in("gala")); return }); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- as(t, pool, user, a, func(tx pgx.Tx) error {
				_, err := catalogue.CreateTicketType(ctx, tx, a, ev.ID, actor, tt("T"))
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var distinct, total int
	if err := testdb.Owner(t, pool).QueryRow(ctx, `select count(distinct position), count(*) from ticket_types where event_id = $1`, ev.ID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != n || distinct != n {
		t.Fatalf("%d ticket types, %d distinct positions", total, distinct)
	}
}
