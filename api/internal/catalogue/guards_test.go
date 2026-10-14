package catalogue_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/system"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func ptr[T any](v T) *T { return &v }

// fixture is a live event with two ticket types, General and VIP, both in
// two quotas, Hall (10) and Venue (100), so any one of them can go and the
// event still has a ticket type with a capacity.
type fixture struct {
	pool, owner      *pgxpool.Pool
	user, org, event uuid.UUID
	general, vip     uuid.UUID
	hall, venue      uuid.UUID
	actor            audit.Actor
}

func seedLiveEvent(t *testing.T) fixture {
	t.Helper()
	pool := testdb.New(t)
	user, orgs := seed(t, pool)
	f := fixture{pool: pool, owner: testdb.Owner(t, pool), user: user, org: orgs[0], actor: audit.Actor{User: user}}
	err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		ev, err := catalogue.CreateEvent(ctx, tx, f.org, f.actor, in("guarded"))
		if err != nil {
			return err
		}
		f.event = ev.ID
		for _, x := range []struct {
			id   *uuid.UUID
			name string
		}{{&f.general, "General"}, {&f.vip, "VIP"}} {
			tt, err := catalogue.CreateTicketType(ctx, tx, f.org, f.event, f.actor, tt(x.name))
			if err != nil {
				return err
			}
			*x.id = tt.ID
		}
		for _, x := range []struct {
			id   *uuid.UUID
			name string
			size int
		}{{&f.hall, "Hall", 10}, {&f.venue, "Venue", 100}} {
			q, err := catalogue.CreateQuota(ctx, tx, f.org, f.event, f.actor, catalogue.QuotaInput{Name: x.name, Size: x.size, TicketTypeIDs: []uuid.UUID{f.general, f.vip}})
			if err != nil {
				return err
			}
			*x.id = q.ID
		}
		_, err = catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{Status: ptr("live")})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) inScope(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	return as(t, f.pool, f.user, f.org, func(tx pgx.Tx) error { return fn(context.Background(), tx) })
}

var orderNo atomic.Int64

// sell records n General tickets the way the sale path will: "hold" is an
// open checkout holding them, "ticket" an order with n valid tickets,
// "expired" a checkout whose hold ran out a minute ago.
func (f fixture) sell(t *testing.T, sale string, n int) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch sale {
	case "hold", "expired":
		expires := "now() + interval '15 minutes'"
		if sale == "expired" {
			expires = "now() - interval '1 minute'"
		}
		_, err = f.owner.Exec(ctx, `with c as (insert into checkouts(organizer_id, event_id, token_hash, expires_at)
				values ($1, $2, sha256(gen_random_uuid()::text::bytea), `+expires+`) returning id)
			insert into holds(organizer_id, event_id, checkout_id, ticket_type_id, quantity) select $1, $2, id, $3, $4 from c`,
			f.org, f.event, f.general, n)
	case "ticket":
		_, err = f.owner.Exec(ctx, `with o as (insert into orders(organizer_id, event_id, code, email, status, subtotal_cents, discount_cents,
				tax_cents, fee_cents, total_cents, fee_mode, quote, details)
				values ($1, $2, $5, 'b@example.com', 'paid', 0, 0, 0, 0, 0, 'pass', '{}', '{}') returning id)
			insert into tickets(organizer_id, event_id, order_id, ticket_type_id, price_cents, discount_cents, tax_cents, fee_cents, secret)
			select $1, $2, o.id, $3, 2500, 0, 0, 0, decode(md5(random()::text), 'hex') from o, generate_series(1, $4)`,
			f.org, f.event, f.general, n, fmt.Sprintf("TS-%05d", orderNo.Add(1)))
	default:
		t.Fatalf("unknown sale %q", sale)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func conflictOf(err error) (code, field string) {
	var c catalogue.ErrConflict
	if errors.As(err, &c) {
		return c.Code, c.Field
	}
	return "", ""
}

var guarded = []struct {
	name        string
	edit        func(ctx context.Context, tx pgx.Tx, f fixture) error
	code, field string
}{
	{"ticket price", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		_, err := catalogue.UpdateTicketType(ctx, tx, f.org, f.event, f.general, f.actor, catalogue.TicketTypePatch{PriceCents: ptr(int64(3000))})
		return err
	}, "has_sales", "price_cents"},
	{"ticket max per order", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		_, err := catalogue.UpdateTicketType(ctx, tx, f.org, f.event, f.general, f.actor, catalogue.TicketTypePatch{MaxPerOrder: ptr(2)})
		return err
	}, "has_sales", "max_per_order"},
	{"delete ticket type", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		return catalogue.DeleteTicketType(ctx, tx, f.org, f.event, f.general, f.actor)
	}, "has_sales", ""},
	{"shrink capacity below sales", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		_, err := catalogue.UpdateQuota(ctx, tx, f.org, f.event, f.hall, f.actor, catalogue.QuotaPatch{Size: ptr(0)})
		return err
	}, "quota_below_sales", "size"},
	{"unlink a sold ticket type", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		_, err := catalogue.UpdateQuota(ctx, tx, f.org, f.event, f.hall, f.actor, catalogue.QuotaPatch{TicketTypeIDs: ptr([]uuid.UUID{f.vip})})
		return err
	}, "has_sales", "ticket_type_ids"},
	{"delete quota", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		return catalogue.DeleteQuota(ctx, tx, f.org, f.event, f.hall, f.actor)
	}, "has_sales", ""},
	{"event tax rate", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{TaxRateBP: ptr(500)})
		return err
	}, "has_sales", "tax_rate_bp"},
	{"event fee mode", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{FeeMode: ptr("absorb")})
		return err
	}, "has_sales", "fee_mode"},
	{"event timezone", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{Timezone: ptr("America/Chicago")})
		return err
	}, "has_sales", "timezone"},
	{"back to draft", func(ctx context.Context, tx pgx.Tx, f fixture) error {
		_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{Status: ptr("draft")})
		return err
	}, "has_sales", "status"},
}

func TestGuardsRefuseEditsOnceSalesExist(t *testing.T) {
	for _, c := range guarded {
		for _, sale := range []string{"hold", "ticket"} {
			t.Run(c.name+" after a "+sale, func(t *testing.T) {
				f := seedLiveEvent(t)
				f.sell(t, sale, 1)
				err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error { return c.edit(ctx, tx, f) })
				if code, field := conflictOf(err); code != c.code || field != c.field {
					t.Fatalf("got %v (field %q), want 409 %s on %q", err, field, c.code, c.field)
				}
			})
		}
	}
}

// The guard isn't blanket: on a fresh live event every one of those works.
func TestEditsStillWorkBeforeAnySale(t *testing.T) {
	for _, c := range guarded {
		t.Run(c.name, func(t *testing.T) {
			f := seedLiveEvent(t)
			if err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error { return c.edit(ctx, tx, f) }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Name, description, sales window and hidden stay editable after a sale.
func TestCosmeticEditsSurviveSales(t *testing.T) {
	f := seedLiveEvent(t)
	f.sell(t, "ticket", 1)
	err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := catalogue.UpdateTicketType(ctx, tx, f.org, f.event, f.general, f.actor,
			catalogue.TicketTypePatch{Name: ptr("Standing"), Description: ptr("Doors at 7"), Hidden: ptr(true)}); err != nil {
			return err
		}
		if _, err := catalogue.UpdateQuota(ctx, tx, f.org, f.event, f.hall, f.actor, catalogue.QuotaPatch{Name: ptr("Main hall")}); err != nil {
			return err
		}
		_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{Name: ptr("Spring Ball")})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCapacityCanGrowAndShrinkToSales(t *testing.T) {
	f := seedLiveEvent(t)
	f.sell(t, "hold", 3)
	resize := func(n int) error {
		return f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
			_, err := catalogue.UpdateQuota(ctx, tx, f.org, f.event, f.hall, f.actor, catalogue.QuotaPatch{Size: ptr(n)})
			return err
		})
	}
	if err := resize(3); err != nil {
		t.Fatalf("10 -> 3 with 3 held: %v", err)
	}
	if err := resize(2); !hasCode(err, "quota_below_sales") {
		t.Fatalf("3 -> 2 with 3 held: %v, want quota_below_sales", err)
	}
	if err := resize(50); err != nil {
		t.Fatalf("3 -> 50: %v", err)
	}
}

func TestExpiredHoldsDontCountAsSales(t *testing.T) {
	f := seedLiveEvent(t)
	f.sell(t, "expired", 4)
	err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := catalogue.UpdateTicketType(ctx, tx, f.org, f.event, f.general, f.actor, catalogue.TicketTypePatch{PriceCents: ptr(int64(3000))}); err != nil {
			return err
		}
		_, err := catalogue.UpdateQuota(ctx, tx, f.org, f.event, f.hall, f.actor, catalogue.QuotaPatch{Size: ptr(0)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// An abandoned cart doesn't pin its ticket type: the type goes, and the
	// expired hold with it.
	if err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		return catalogue.DeleteTicketType(ctx, tx, f.org, f.event, f.general, f.actor)
	}); err != nil {
		t.Fatalf("delete with only an expired hold: %v", err)
	}
	var holds int
	if err := f.owner.QueryRow(context.Background(), `select count(*) from holds`).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("holds left: %d %v", holds, err)
	}
	// A ticket still pins it.
	g := seedLiveEvent(t)
	g.sell(t, "expired", 1)
	g.sell(t, "ticket", 1)
	if err := g.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		return catalogue.DeleteTicketType(ctx, tx, g.org, g.event, g.general, g.actor)
	}); !hasCode(err, "has_sales") {
		t.Fatalf("delete with a ticket: %v, want has_sales", err)
	}
}

func hasCode(err error, code string) bool {
	c, _ := conflictOf(err)
	return c == code
}

// Linking a ticket type into a quota brings its sales with it: the quota
// can't end up smaller than what it now holds, whether it's new or patched.
func TestLinkChangesRespectSales(t *testing.T) {
	f := seedLiveEvent(t)
	f.sell(t, "hold", 3)
	var tiny catalogue.Quota
	if err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) (err error) {
		tiny, err = catalogue.CreateQuota(ctx, tx, f.org, f.event, f.actor, catalogue.QuotaInput{Name: "Tiny", Size: 5, TicketTypeIDs: []uuid.UUID{f.vip}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	patch := func(p catalogue.QuotaPatch) error {
		return f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
			_, err := catalogue.UpdateQuota(ctx, tx, f.org, f.event, tiny.ID, f.actor, p)
			return err
		})
	}
	create := func(size int) error {
		return f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
			_, err := catalogue.CreateQuota(ctx, tx, f.org, f.event, f.actor, catalogue.QuotaInput{Name: "Small", Size: size, TicketTypeIDs: []uuid.UUID{f.general}})
			return err
		})
	}
	both := []uuid.UUID{f.general, f.vip}
	if err := patch(catalogue.QuotaPatch{Size: ptr(2), TicketTypeIDs: &both}); !hasCode(err, "quota_below_sales") {
		t.Fatalf("shrink to 2 and add General (3 held): %v, want quota_below_sales", err)
	}
	if err := patch(catalogue.QuotaPatch{Size: ptr(4), TicketTypeIDs: &both}); err != nil {
		t.Fatalf("shrink to 4 and add General: %v", err)
	}
	if err := patch(catalogue.QuotaPatch{TicketTypeIDs: &[]uuid.UUID{f.general}}); err != nil {
		t.Fatalf("drop VIP, which has no sales: %v", err)
	}
	if err := create(2); !hasCode(err, "quota_below_sales") {
		t.Fatalf("new quota of 2 over 3 held: %v, want quota_below_sales", err)
	}
	if err := create(3); err != nil {
		t.Fatalf("new quota of 3 over 3 held: %v", err)
	}
}

// A quota created while a checkout is holding tickets of its type waits for
// the checkout and then counts its hold: it can't be born oversold.
func TestNewQuotaRacesWithAHold(t *testing.T) {
	f := seedLiveEvent(t)
	ctx := context.Background()
	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- system.Run(ctx, f.pool, f.org, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select 1 from quotas where id in (select quota_id from quota_ticket_types where ticket_type_id = $1)
				order by id for update`, f.general); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `with c as (insert into checkouts(organizer_id, event_id, token_hash, expires_at)
					values ($1, $2, sha256('race'), now() + interval '15 minutes') returning id)
				insert into holds(organizer_id, event_id, checkout_id, ticket_type_id, quantity) select $1, $2, id, $3, 3 from c`,
				f.org, f.event, f.general); err != nil {
				return err
			}
			close(held)
			time.Sleep(300 * time.Millisecond)
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("checkout: %v", err)
	}
	err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := catalogue.CreateQuota(ctx, tx, f.org, f.event, f.actor, catalogue.QuotaInput{Name: "Small", Size: 2, TicketTypeIDs: []uuid.UUID{f.general}})
		return err
	})
	if err := <-done; err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if !hasCode(err, "quota_below_sales") {
		t.Fatalf("quota racing a hold: %v, want quota_below_sales", err)
	}
}

// An order with no tickets (cancelled before any were cut) still means the
// event has sold: it can't go back to draft, and a draft with one can't be
// deleted (409, not a foreign key 500).
func TestOrdersCountAsSales(t *testing.T) {
	f := seedLiveEvent(t)
	ctx := context.Background()
	order := func(event uuid.UUID, code string) {
		t.Helper()
		if _, err := f.owner.Exec(ctx, `insert into orders(organizer_id, event_id, code, email, status, subtotal_cents, discount_cents,
				tax_cents, fee_cents, total_cents, fee_mode, quote, details)
			values ($1, $2, $3, 'b@example.com', 'cancelled', 0, 0, 0, 0, 0, 'pass', '{}', '{}')`, f.org, event, code); err != nil {
			t.Fatal(err)
		}
	}
	order(f.event, "TS-CCCCC")
	if err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{Status: ptr("draft")})
		return err
	}); !hasCode(err, "has_sales") {
		t.Fatalf("live -> draft with a cancelled order: %v, want has_sales", err)
	}
	var draft catalogue.Event
	if err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) (err error) {
		draft, err = catalogue.CreateEvent(ctx, tx, f.org, f.actor, in("draft-with-order"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	order(draft.ID, "TS-DDDDD")
	if err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		return catalogue.DeleteEvent(ctx, tx, f.org, draft.ID, f.actor)
	}); !hasCode(err, "has_sales") {
		t.Fatalf("delete a draft with an order: %v, want has_sales", err)
	}
}

func TestPromoAndQuestionGuards(t *testing.T) {
	f := seedLiveEvent(t)
	ctx := context.Background()
	var used, held catalogue.PromoCode
	var asked catalogue.Question
	if err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) (err error) {
		if used, err = catalogue.CreatePromo(ctx, tx, f.org, f.event, f.actor, catalogue.PromoInput{Code: "USED", Kind: "percent", Value: 1000}); err != nil {
			return err
		}
		if held, err = catalogue.CreatePromo(ctx, tx, f.org, f.event, f.actor, catalogue.PromoInput{Code: "HELD", Kind: "percent", Value: 1000}); err != nil {
			return err
		}
		asked, err = catalogue.CreateQuestion(ctx, tx, f.org, f.event, f.actor, catalogue.QuestionInput{Label: "Diet", Kind: "text", Scope: "order"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `update promo_codes set used_count = 1 where id = $1`, used.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `insert into checkouts(organizer_id, event_id, token_hash, promo_code_id, expires_at)
		values ($1, $2, sha256('held'), $3, now() + interval '15 minutes')`, f.org, f.event, held.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `with o as (insert into orders(organizer_id, event_id, code, email, subtotal_cents, discount_cents,
			tax_cents, fee_cents, total_cents, fee_mode, quote, details)
			values ($1, $2, 'TS-QQQQQ', 'b@example.com', 0, 0, 0, 0, 0, 'pass', '{}', '{}') returning id)
		insert into answers(organizer_id, event_id, order_id, question_id, value) select $1, $2, id, $3, 'None' from o`, f.org, f.event, asked.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []catalogue.PromoCode{used, held} {
		edits := map[string]func(ctx context.Context, tx pgx.Tx) error{
			"delete": func(ctx context.Context, tx pgx.Tx) error {
				return catalogue.DeletePromo(ctx, tx, f.org, f.event, p.ID, f.actor)
			},
			"value": func(ctx context.Context, tx pgx.Tx) error {
				_, err := catalogue.UpdatePromo(ctx, tx, f.org, f.event, p.ID, f.actor, catalogue.PromoPatch{Value: ptr(int64(2000))})
				return err
			},
			"kind": func(ctx context.Context, tx pgx.Tx) error {
				_, err := catalogue.UpdatePromo(ctx, tx, f.org, f.event, p.ID, f.actor, catalogue.PromoPatch{Kind: ptr("amount"), Value: ptr(int64(500))})
				return err
			},
		}
		for name, edit := range edits {
			if code, _ := conflictOf(f.inScope(t, edit)); code != "has_sales" {
				t.Errorf("%s %s: got %q, want has_sales", p.Code, name, code)
			}
		}
		// The rest of the code stays editable.
		if err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
			_, err := catalogue.UpdatePromo(ctx, tx, f.org, f.event, p.ID, f.actor, catalogue.PromoPatch{MaxUses: nullable.NewNullableWithValue(50)})
			return err
		}); err != nil {
			t.Errorf("%s max uses: %v", p.Code, err)
		}
	}
	err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		return catalogue.DeleteQuestion(ctx, tx, f.org, f.event, asked.ID, f.actor)
	})
	if code, _ := conflictOf(err); code != "has_sales" {
		t.Fatalf("delete answered question: %v, want has_sales", err)
	}
}

// A checkout that takes the quota lock first makes the price edit wait, and
// the edit then sees the hold: never a silent price change under a cart.
func TestGuardsRaceWithAHold(t *testing.T) {
	f := seedLiveEvent(t)
	ctx := context.Background()
	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- system.Run(ctx, f.pool, f.org, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select 1 from quotas where event_id = $1 order by id for update`, f.event); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `with c as (insert into checkouts(organizer_id, event_id, token_hash, expires_at)
					values ($1, $2, sha256('race'), now() + interval '15 minutes') returning id)
				insert into holds(organizer_id, event_id, checkout_id, ticket_type_id, quantity) select $1, $2, id, $3, 1 from c`,
				f.org, f.event, f.general); err != nil {
				return err
			}
			close(held)
			time.Sleep(300 * time.Millisecond)
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("checkout: %v", err)
	}
	err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := catalogue.UpdateTicketType(ctx, tx, f.org, f.event, f.general, f.actor, catalogue.TicketTypePatch{PriceCents: ptr(int64(3000))})
		return err
	})
	if err := <-done; err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if code, _ := conflictOf(err); code != "has_sales" {
		t.Fatalf("price edit racing a hold: %v, want has_sales", err)
	}
}

// An event edit holds the event row while a checkout holds the quotas and
// inserts its cart (a foreign key share lock on the event). The edit then
// needs the quotas. With FOR UPDATE on the event that is a deadlock; with
// FOR NO KEY UPDATE the cart goes in and the edit sees it.
func TestEventEditAndAHoldDontDeadlock(t *testing.T) {
	f := seedLiveEvent(t)
	ctx := context.Background()
	editing, locked := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-editing
		done <- system.Run(ctx, f.pool, f.org, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `select 1 from quotas where event_id = $1 order by id for update`, f.event); err != nil {
				return err
			}
			close(locked)
			if _, err := tx.Exec(ctx, `with c as (insert into checkouts(organizer_id, event_id, token_hash, expires_at)
					values ($1, $2, sha256('race'), now() + interval '15 minutes') returning id)
				insert into holds(organizer_id, event_id, checkout_id, ticket_type_id, quantity) select $1, $2, id, $3, 1 from c`,
				f.org, f.event, f.general); err != nil {
				return err
			}
			time.Sleep(300 * time.Millisecond)
			return nil
		})
	}()
	err := f.inScope(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{Name: ptr("Spring Ball")}); err != nil {
			return err
		}
		close(editing)
		select {
		case <-locked:
		case err := <-done:
			return fmt.Errorf("checkout ended early: %w", err)
		}
		time.Sleep(50 * time.Millisecond) // let the checkout reach its insert
		_, err := catalogue.UpdateEvent(ctx, tx, f.org, f.event, f.actor, catalogue.EventPatch{TaxRateBP: ptr(500)})
		return err
	})
	if cerr := <-done; cerr != nil {
		t.Fatalf("checkout: %v (edit: %v)", cerr, err)
	}
	if code, _ := conflictOf(err); code != "has_sales" {
		t.Fatalf("event edit racing a hold: %v, want has_sales", err)
	}
}
