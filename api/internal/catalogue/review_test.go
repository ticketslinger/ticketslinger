package catalogue_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func TestTicketTypeAndQuotaNamesAreTrimmed(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	var made catalogue.TicketType
	if err := e.run(e.a, func(tx pgx.Tx) (err error) {
		made, err = catalogue.CreateTicketType(ctx, tx, e.a, e.ev.ID, e.actor, tt("  Balcony \t"))
		return
	}); err != nil || made.Name != "Balcony" {
		t.Fatalf("ticket type: %v %q", err, made.Name)
	}
	blank := " \t "
	err := e.run(e.a, func(tx pgx.Tx) error {
		_, err := catalogue.CreateTicketType(ctx, tx, e.a, e.ev.ID, e.actor, tt(blank))
		return err
	})
	if !invalidCode(err, "invalid_name") {
		t.Errorf("blank ticket type: %v", err)
	}
	err = e.run(e.a, func(tx pgx.Tx) error {
		_, err := catalogue.UpdateTicketType(ctx, tx, e.a, e.ev.ID, made.ID, e.actor, catalogue.TicketTypePatch{Name: &blank})
		return err
	})
	if !invalidCode(err, "invalid_name") {
		t.Errorf("blank ticket type patch: %v", err)
	}

	q, err := e.create(t, "  Floor  ", 10)
	if err != nil || q.Name != "Floor" {
		t.Fatalf("quota: %v %q", err, q.Name)
	}
	if _, err := e.create(t, blank, 10); !invalidCode(err, "invalid_name") {
		t.Errorf("blank quota: %v", err)
	}
	if _, err := e.patch(q.ID, catalogue.QuotaPatch{Name: &blank}); !invalidCode(err, "invalid_name") {
		t.Errorf("blank quota patch: %v", err)
	}
}

// A zero discount is an access code: it has to unlock something.
func TestPromoValueZeroOnlyUnlocks(t *testing.T) {
	e := newQEnv(t)
	secret := e.hidden(t, "Secret")
	for _, kind := range []string{"percent", "amount"} {
		if _, err := e.promo(pc("FREE-"+kind, kind, 0)); !invalidCode(err, "bad_discount") {
			t.Errorf("%s 0 without unlocks: %v", kind, err)
		}
	}
	in := pc("ACCESS", "percent", 0)
	in.Unlocks = []uuid.UUID{secret.ID}
	p, err := e.promo(in)
	if err != nil || p.Value != 0 || len(p.Unlocks) != 1 {
		t.Fatalf("access code: %v %+v", err, p)
	}
	if _, err := e.patchPromo(p.ID, catalogue.PromoPatch{Unlocks: &[]uuid.UUID{}}); !invalidCode(err, "bad_discount") {
		t.Errorf("emptying unlocks on a 0 code: %v", err)
	}
	ten := int64(1000)
	if up, err := e.patchPromo(p.ID, catalogue.PromoPatch{Value: &ten, Unlocks: &[]uuid.UUID{}}); err != nil || up.Value != 1000 || len(up.Unlocks) != 0 {
		t.Errorf("a real discount needs no unlocks: %v %+v", err, up)
	}
	// The CHECK in 00015 still refuses a negative value.
	var pg *pgconn.PgError
	err = e.run(e.a, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `update promo_codes set value = -1 where id = $1`, p.ID)
		return err
	})
	if !asPg(err, &pg) || pg.Code != "23514" {
		t.Fatalf("raw negative value: %v", err)
	}
}

// Child writes take FOR NO KEY UPDATE on the event, so a transaction holding
// FOR KEY SHARE on it (what an FK insert from a hold or an order takes) doesn't
// block them.
func TestChildWritesDoNotWaitOnKeyShare(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	owner := testdb.Owner(t, e.pool)
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select 1 from events where id = $1 for key share`, e.ev.ID); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err = e.run(e.a, func(tx pgx.Tx) error {
		_, err := catalogue.CreateQuota(short, tx, e.a, e.ev.ID, e.actor, catalogue.QuotaInput{Name: "Floor", Size: 5})
		return err
	})
	if err != nil {
		t.Fatalf("quota while an event key share is held: %v", err)
	}
}

func TestTicketTypeDeletedAuditListsQuotas(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	q, err := e.create(t, "Floor", 10, e.general.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) error {
		return catalogue.DeleteTicketType(ctx, tx, e.a, e.ev.ID, e.general.ID, e.actor)
	}); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := testdb.Owner(t, e.pool).QueryRow(ctx, `select before from audit_log where action = 'ticket_type.deleted' and entity_id = $1`, e.general.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Name     string      `json:"name"`
		QuotaIDs []uuid.UUID `json:"quota_ids"`
	}
	if err := json.Unmarshal(before, &got); err != nil || got.Name != "General" || len(got.QuotaIDs) != 1 || got.QuotaIDs[0] != q.ID {
		t.Fatalf("before: %v %s", err, before)
	}
}
