package sales_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func typeIDs(ev sales.StorefrontEvent) []uuid.UUID {
	var ids []uuid.UUID
	for _, tt := range ev.TicketTypes {
		ids = append(ids, tt.ID)
	}
	return ids
}

func questionIDs(ev sales.StorefrontEvent) []uuid.UUID {
	var ids []uuid.UUID
	for _, q := range ev.Questions {
		ids = append(ids, q.ID)
	}
	return ids
}

func TestStorefrontShowsOnlyLiveEvents(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	ev, err := s.StorefrontEvent(ctx, "acme", "gala", "")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Organizer.Slug != "acme" || ev.Organizer.Name != "Acme" || ev.Event.ID != f.event || ev.Event.Name != "Gala" ||
		ev.Event.State != "on_sale" || ev.Event.FeeMode != "pass" || ev.Event.Timezone != "America/New_York" || ev.Promo != nil {
		t.Fatalf("storefront = %+v", ev)
	}
	f.exec(t, `update events set status = 'draft' where id = $1`, f.event)
	if _, err := s.StorefrontEvent(ctx, "acme", "gala", ""); !errors.Is(err, errNotFound) {
		t.Fatalf("draft: %v, want ErrNotFound", err)
	}
	if _, err := s.StorefrontEvent(ctx, "acme", "nope", ""); !errors.Is(err, errNotFound) {
		t.Fatalf("unknown event: %v, want ErrNotFound", err)
	}
}

// Hidden ticket types, and questions asked only of them, show only with a
// code that unlocks them.
func TestStorefrontHiddenTypesNeedTheirCode(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	vip := f.addVIP(t, 9000, true)
	everyone := f.question(t, "Name", "text", "ticket", true, nil)
	vipOnly := f.question(t, "Dinner choice", "select", "ticket", false, []string{"Fish", "Veg"}, vip)
	shared := f.question(t, "Parking", "checkbox", "order", false, nil, vip, f.general)
	backstage := f.promo(t, "BACKSTAGE", "percent", 0)
	f.exec(t, `insert into promo_unlocks(organizer_id, event_id, promo_code_id, ticket_type_id) values ($1, $2, $3, $4)`, f.org, f.event, backstage, vip)
	f.promo(t, "TENOFF", "percent", 1000)

	for _, code := range []string{"", "TENOFF", "NOSUCH"} {
		ev, err := s.StorefrontEvent(ctx, "acme", "gala", code)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(typeIDs(ev), []uuid.UUID{f.general}) || ev.TicketTypes[0].Unlocked {
			t.Fatalf("code %q: types %v, want General only", code, typeIDs(ev))
		}
		if !slices.Equal(questionIDs(ev), []uuid.UUID{everyone, shared}) || !slices.Equal(ev.Questions[1].AppliesTo, []uuid.UUID{f.general}) {
			t.Fatalf("code %q: questions %+v, want Name and Parking (General only)", code, ev.Questions)
		}
		if (code == "TENOFF") != (ev.Promo != nil) {
			t.Fatalf("code %q: promo %+v", code, ev.Promo)
		}
	}
	ev, err := s.StorefrontEvent(ctx, "acme", "gala", "backstage")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(typeIDs(ev), []uuid.UUID{f.general, vip}) || ev.TicketTypes[0].Unlocked || !ev.TicketTypes[1].Unlocked {
		t.Fatalf("with the code: %+v, want General, then VIP unlocked", ev.TicketTypes)
	}
	if ev.Promo == nil || *ev.Promo != (sales.StorefrontPromo{Code: "BACKSTAGE", Kind: "percent", Value: 0}) {
		t.Fatalf("promo = %+v", ev.Promo)
	}
	if !slices.Equal(questionIDs(ev), []uuid.UUID{everyone, vipOnly, shared}) || !slices.Equal(ev.Questions[1].Options, []string{"Fish", "Veg"}) ||
		!slices.Equal(ev.Questions[1].AppliesTo, []uuid.UUID{vip}) || len(ev.Questions[2].AppliesTo) != 2 {
		t.Fatalf("questions with the code: %+v", ev.Questions)
	}
	// A code past its window unlocks nothing.
	f.exec(t, `update promo_codes set valid_until = now() - interval '1 minute', valid_from = now() - interval '1 day' where id = $1`, backstage)
	if ev, err := s.StorefrontEvent(ctx, "acme", "gala", "BACKSTAGE"); err != nil || len(ev.TicketTypes) != 1 || ev.Promo != nil {
		t.Fatalf("expired code: %+v %v", ev, err)
	}
}

func TestStorefrontRemainingAndSaleState(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	ctx := context.Background()
	soon := f.addVIP(t, 9000, false)
	f.exec(t, `update ticket_types set sales_start = now() + interval '1 day' where id = $1`, soon)
	f.open(t, s, buy(sales.Item{TicketTypeID: f.general, Quantity: 3}))
	ev, err := s.StorefrontEvent(ctx, "acme", "gala", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range ev.TicketTypes {
		want := f.count(t, `select remaining from event_availability($1) where ticket_type_id = $2`, f.event, tt.ID)
		if tt.Remaining != want || tt.Remaining != 7 {
			t.Fatalf("%s: remaining %d, want %d (7)", tt.Name, tt.Remaining, want)
		}
	}
	if !ev.TicketTypes[0].OnSale || ev.TicketTypes[1].OnSale || ev.TicketTypes[1].SalesStart == nil {
		t.Fatalf("on sale = %v, %v; want General yes, VIP not yet", ev.TicketTypes[0].OnSale, ev.TicketTypes[1].OnSale)
	}
	f.exec(t, `update events set starts_at = now() - interval '2 days', ends_at = now() - interval '1 day' where id = $1`, f.event)
	ev, err = s.StorefrontEvent(ctx, "acme", "gala", "")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event.State != "ended" {
		t.Fatalf("state %q, want ended", ev.Event.State)
	}
	for _, tt := range ev.TicketTypes {
		if tt.OnSale {
			t.Fatalf("%s on sale after the event", tt.Name)
		}
	}
}
