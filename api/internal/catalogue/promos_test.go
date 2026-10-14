package catalogue_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oapi-codegen/nullable"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func (e *qenv) hidden(t *testing.T, name string) (h catalogue.TicketType) {
	t.Helper()
	in := tt(name)
	in.Hidden = true
	if err := e.run(e.a, func(tx pgx.Tx) (err error) {
		h, err = catalogue.CreateTicketType(context.Background(), tx, e.a, e.ev.ID, e.actor, in)
		return
	}); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *qenv) promo(in catalogue.PromoInput) (p catalogue.PromoCode, err error) {
	err = e.run(e.a, func(tx pgx.Tx) (x error) {
		p, x = catalogue.CreatePromo(context.Background(), tx, e.a, e.ev.ID, e.actor, in)
		return
	})
	return
}

func (e *qenv) patchPromo(id uuid.UUID, p catalogue.PromoPatch) (out catalogue.PromoCode, err error) {
	err = e.run(e.a, func(tx pgx.Tx) (x error) {
		out, x = catalogue.UpdatePromo(context.Background(), tx, e.a, e.ev.ID, id, e.actor, p)
		return
	})
	return
}

func pc(code, kind string, value int64) catalogue.PromoInput {
	return catalogue.PromoInput{Code: code, Kind: kind, Value: value}
}

func conflictCode(err error, code string) bool {
	var c catalogue.ErrConflict
	return errors.As(err, &c) && c.Code == code
}

func TestPromoRejectsForeignTicketTypes(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	secret := e.hidden(t, "Secret")
	var other catalogue.TicketType
	if err := e.run(e.a, func(tx pgx.Tx) error {
		ev2, err := catalogue.CreateEvent(ctx, tx, e.a, e.actor, in("second"))
		if err != nil {
			return err
		}
		in := tt("Elsewhere")
		in.Hidden = true
		other, err = catalogue.CreateTicketType(ctx, tx, e.a, ev2.ID, e.actor, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{e.foreign.ID, other.ID} {
		for _, c := range []catalogue.PromoInput{
			{Code: "A1B", Kind: "percent", Value: 100, AppliesTo: []uuid.UUID{e.general.ID, id}},
			{Code: "A1B", Kind: "percent", Value: 100, Unlocks: []uuid.UUID{id}},
		} {
			if _, err := e.promo(c); !invalidCode(err, "unknown_ticket_type") {
				t.Fatalf("create with %v: %v", id, err)
			}
		}
	}
	p, err := e.promo(catalogue.PromoInput{Code: "SECRET", Kind: "percent", Value: 100, Unlocks: []uuid.UUID{secret.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range []catalogue.PromoPatch{{AppliesTo: &[]uuid.UUID{e.foreign.ID}}, {Unlocks: &[]uuid.UUID{other.ID}}} {
		if _, err := e.patchPromo(p.ID, patch); !invalidCode(err, "unknown_ticket_type") {
			t.Fatalf("patch: %v", err)
		}
	}
	var n int
	if err := testdb.Owner(t, e.pool).QueryRow(ctx, `select (select count(*) from promo_applies_to) + (select count(*) from promo_unlocks where ticket_type_id <> $1)
		+ (select count(*) from promo_codes where code = 'A1B')`, secret.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stray rows: %d %v", n, err)
	}
	// Bypassing Go as the app role: a cross-event link dies on the declarative FK.
	var pg *pgconn.PgError
	for _, table := range []string{"promo_applies_to", "promo_unlocks"} {
		err := e.run(e.a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `insert into `+table+`(organizer_id, event_id, promo_code_id, ticket_type_id) values ($1, $2, $3, $4)`, e.a, p.EventID, p.ID, other.ID)
			return err
		})
		if !asPg(err, &pg) || pg.Code != "23503" {
			t.Fatalf("%s cross-event link: %v", table, err)
		}
		if err := e.run(e.a, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `insert into `+table+`(organizer_id, event_id, promo_code_id, ticket_type_id) values ($1, $2, $3, $4)`, e.a, p.EventID, p.ID, e.foreign.ID)
			return err
		}); err == nil {
			t.Fatalf("%s cross-tenant link accepted", table)
		}
	}
}

func TestPromoValidationAndConflicts(t *testing.T) {
	e := newQEnv(t)
	zero, neg := 0, -1
	now := time.Now()
	earlier := now.Add(-time.Hour)
	for _, c := range []struct {
		in   catalogue.PromoInput
		code string
	}{
		{pc("ab", "percent", 100), "bad_code"},
		{pc("has space", "percent", 100), "bad_code"},
		{pc(strings.Repeat("A", 33), "percent", 100), "bad_code"},
		{pc("LOW_ER", "percent", 100), "bad_code"},
		{pc("OK1", "free", 100), "bad_kind"},
		{pc("OK1", "percent", 0), "bad_discount"},
		{pc("OK1", "percent", 10001), "bad_discount"},
		{pc("OK1", "amount", 0), "bad_discount"},
		{pc("OK1", "amount", 10000001), "bad_discount"},
		{catalogue.PromoInput{Code: "OK1", Kind: "amount", Value: 5, MaxUses: &zero}, "bad_max_uses"},
		{catalogue.PromoInput{Code: "OK1", Kind: "amount", Value: 5, MaxUses: &neg}, "bad_max_uses"},
		{catalogue.PromoInput{Code: "OK1", Kind: "amount", Value: 5, ValidFrom: &now, ValidUntil: &earlier}, "bad_validity"},
		{catalogue.PromoInput{Code: "OK1", Kind: "amount", Value: 5, ValidFrom: &now, ValidUntil: &now}, "bad_validity"},
	} {
		if _, err := e.promo(c.in); !invalidCode(err, c.code) {
			t.Errorf("%+v: want %s, got %v", c.in, c.code, err)
		}
	}
	p, err := e.promo(pc("  early-bird ", "percent", 10000))
	if err != nil || p.Code != "EARLY-BIRD" || p.UsedCount != 0 || p.AppliesTo == nil || p.Unlocks == nil {
		t.Fatalf("create: %v %+v", err, p)
	}
	if _, err := e.promo(pc("Early-Bird", "amount", 10000000)); !conflictCode(err, "code_taken") {
		t.Fatalf("duplicate: %v", err)
	}
	// The same code on another event is fine; renaming onto a taken code is not.
	other, err := e.promo(pc("OTHER", "amount", 500))
	if err != nil {
		t.Fatal(err)
	}
	taken := "early-bird"
	if _, err := e.patchPromo(other.ID, catalogue.PromoPatch{Code: &taken}); !conflictCode(err, "code_taken") {
		t.Fatalf("rename onto taken: %v", err)
	}
	// Direct SQL as the app role: the CHECKs back the Go rules up.
	var pg *pgconn.PgError
	err = e.run(e.a, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `update promo_codes set code = 'lower' where id = $1`, p.ID)
		return err
	})
	if !asPg(err, &pg) || pg.Code != "23514" {
		t.Fatalf("raw lower-case code: %v", err)
	}
}

func TestPromoUnlocksOnlyHiddenAndPatchSemantics(t *testing.T) {
	e := newQEnv(t)
	secret := e.hidden(t, "Secret")
	if _, err := e.promo(catalogue.PromoInput{Code: "VIS", Kind: "percent", Value: 100, Unlocks: []uuid.UUID{e.general.ID}}); !invalidCode(err, "unlock_not_hidden") {
		t.Fatalf("visible unlock: %v", err)
	} else if ie := err.(catalogue.ErrInvalid); !strings.Contains(ie.Message, "“General”") {
		t.Fatalf("message should name the ticket: %q", ie.Message)
	}
	max := 5
	from := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	p, err := e.promo(catalogue.PromoInput{Code: "VIP", Kind: "amount", Value: 500, MaxUses: &max, ValidFrom: &from,
		AppliesTo: []uuid.UUID{e.vip.ID, e.vip.ID}, Unlocks: []uuid.UUID{secret.ID, secret.ID}})
	if err != nil || len(p.AppliesTo) != 1 || len(p.Unlocks) != 1 || p.MaxUses == nil || *p.MaxUses != 5 {
		t.Fatalf("create: %v %+v", err, p)
	}
	// Absent keeps everything; used_count has no way in.
	val := int64(700)
	up, err := e.patchPromo(p.ID, catalogue.PromoPatch{Value: &val})
	if err != nil || up.Value != 700 || len(up.AppliesTo) != 1 || len(up.Unlocks) != 1 || up.MaxUses == nil || up.ValidFrom == nil || !up.UpdatedAt.After(p.UpdatedAt) {
		t.Fatalf("value only: %v %+v", err, up)
	}
	// No-op writes nothing.
	same, err := e.patchPromo(p.ID, catalogue.PromoPatch{Value: &val, AppliesTo: &[]uuid.UUID{e.vip.ID}})
	if err != nil || !same.UpdatedAt.Equal(up.UpdatedAt) {
		t.Fatalf("no-op: %v", err)
	}
	var n int
	if err := testdb.Owner(t, e.pool).QueryRow(context.Background(), `select count(*) from audit_log where action = 'promo_code.updated'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows after no-op: %d %v", n, err)
	}
	// Explicit null clears; empty list clears a set.
	up, err = e.patchPromo(p.ID, catalogue.PromoPatch{MaxUses: nullable.NewNullNullable[int](), ValidFrom: nullable.NewNullNullable[time.Time](),
		AppliesTo: &[]uuid.UUID{}, Unlocks: &[]uuid.UUID{}})
	if err != nil || up.MaxUses != nil || up.ValidFrom != nil || len(up.AppliesTo) != 0 || len(up.Unlocks) != 0 {
		t.Fatalf("clear: %v %+v", err, up)
	}
	if _, err := e.patchPromo(p.ID, catalogue.PromoPatch{Unlocks: &[]uuid.UUID{e.vip.ID}}); !invalidCode(err, "unlock_not_hidden") {
		t.Fatalf("patch visible unlock: %v", err)
	}
	bad := int64(0)
	if _, err := e.patchPromo(p.ID, catalogue.PromoPatch{Value: &bad}); !invalidCode(err, "bad_discount") {
		t.Fatalf("patch bad discount: %v", err)
	}
	// A ticket type a code unlocks can't become visible.
	if _, err := e.patchPromo(p.ID, catalogue.PromoPatch{Unlocks: &[]uuid.UUID{secret.ID}}); err != nil {
		t.Fatal(err)
	}
	show := false
	err = e.run(e.a, func(tx pgx.Tx) error {
		_, err := catalogue.UpdateTicketType(context.Background(), tx, e.a, e.ev.ID, secret.ID, e.actor, catalogue.TicketTypePatch{Hidden: &show})
		return err
	})
	var c catalogue.ErrConflict
	if !errors.As(err, &c) || c.Code != "unlocked_by_promo" || !strings.Contains(c.Message, "VIP") {
		t.Fatalf("show unlocked ticket: %v", err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) error {
		return catalogue.DeletePromo(context.Background(), tx, e.a, e.ev.ID, p.ID, e.actor)
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) error {
		_, err := catalogue.UpdateTicketType(context.Background(), tx, e.a, e.ev.ID, secret.ID, e.actor, catalogue.TicketTypePatch{Hidden: &show})
		return err
	}); err != nil {
		t.Fatalf("show after promo gone: %v", err)
	}
	var created, deleted int
	if err := testdb.Owner(t, e.pool).QueryRow(context.Background(), `select count(*) filter (where action = 'promo_code.created' and after::text like '%applies_to%'),
		count(*) filter (where action = 'promo_code.deleted' and before::text like '%unlocks%') from audit_log`).Scan(&created, &deleted); err != nil || created != 1 || deleted != 1 {
		t.Fatalf("audit: %d %d %v", created, deleted, err)
	}
}

func TestDeletingTicketTypeUnlinksPromos(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	secret := e.hidden(t, "Secret")
	p, err := e.promo(catalogue.PromoInput{Code: "BOTH", Kind: "percent", Value: 1000,
		AppliesTo: []uuid.UUID{e.general.ID, secret.ID}, Unlocks: []uuid.UUID{secret.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) error {
		return catalogue.DeleteTicketType(ctx, tx, e.a, e.ev.ID, secret.ID, e.actor)
	}); err != nil {
		t.Fatalf("delete referenced ticket type: %v", err)
	}
	var got catalogue.PromoCode
	if err := e.run(e.a, func(tx pgx.Tx) (err error) { got, err = catalogue.GetPromo(ctx, tx, e.ev.ID, p.ID); return }); err != nil {
		t.Fatal(err)
	}
	if len(got.AppliesTo) != 1 || got.AppliesTo[0] != e.general.ID || len(got.Unlocks) != 0 {
		t.Fatalf("links after delete: %+v", got)
	}
	// Deleting the event takes the codes with it.
	if err := e.run(e.a, func(tx pgx.Tx) error { return catalogue.DeleteEvent(ctx, tx, e.a, e.ev.ID, e.actor) }); err != nil {
		t.Fatal(err)
	}
}
