package catalogue_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
	"github.com/ticketslinger/ticketslinger/api/migrations"
)

func (e *qenv) reorderTT(ids []uuid.UUID) (out []catalogue.TicketType, err error) {
	err = e.run(e.a, func(tx pgx.Tx) (x error) {
		out, x = catalogue.ReorderTicketTypes(context.Background(), tx, e.a, e.ev.ID, e.actor, ids)
		return
	})
	return
}

func (e *qenv) reorderQ(ids []uuid.UUID) (out []catalogue.Question, err error) {
	err = e.run(e.a, func(tx pgx.Tx) (x error) {
		out, x = catalogue.ReorderQuestions(context.Background(), tx, e.a, e.ev.ID, e.actor, ids)
		return
	})
	return
}

func (e *qenv) auditCount(t *testing.T, action string) (n int) {
	t.Helper()
	if err := testdb.Owner(t, e.pool).QueryRow(context.Background(), `select count(*) from audit_log where action = $1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return
}

func TestReorderTicketTypes(t *testing.T) {
	e := newQEnv(t)
	third, err := e.createTT("Student")
	if err != nil {
		t.Fatal(err)
	}
	if third.Position != 3 {
		t.Fatalf("create appends: position %d, want 3", third.Position)
	}
	want := []uuid.UUID{third.ID, e.general.ID, e.vip.ID}
	list, err := e.reorderTT(want)
	if err != nil || len(list) != 3 || list[0].ID != third.ID || list[1].ID != e.general.ID || list[2].ID != e.vip.ID || list[0].Position != 1 || list[2].Position != 3 {
		t.Fatalf("reorder: %v %+v", err, list)
	}
	if n := e.auditCount(t, "ticket_types.reordered"); n != 1 {
		t.Fatalf("audit entries: %d, want 1", n)
	}
	// Same order again writes nothing.
	if _, err := e.reorderTT(want); err != nil {
		t.Fatal(err)
	}
	if n := e.auditCount(t, "ticket_types.reordered"); n != 1 {
		t.Fatalf("no-op audited: %d entries", n)
	}
	// A new ticket type lands after the reordered ones.
	if d, err := e.createTT("Late"); err != nil || d.Position != 4 {
		t.Fatalf("append after reorder: %v %+v", err, d)
	}
	for name, ids := range map[string][]uuid.UUID{
		"missing":               {e.general.ID, e.vip.ID},
		"extra":                 {e.general.ID, e.vip.ID, third.ID, uuid.New()},
		"duplicate":             {e.general.ID, e.general.ID, e.vip.ID, third.ID},
		"swapped in a stranger": {e.general.ID, e.vip.ID, e.foreign.ID, third.ID},
		"empty":                 {},
	} {
		if _, err := e.reorderTT(ids); !invalidCode(err, "order_mismatch") {
			t.Errorf("%s: got %v, want order_mismatch", name, err)
		}
	}
}

func (e *qenv) createTT(name string) (x catalogue.TicketType, err error) {
	err = e.run(e.a, func(tx pgx.Tx) (y error) {
		x, y = catalogue.CreateTicketType(context.Background(), tx, e.a, e.ev.ID, e.actor, tt(name))
		return
	})
	return
}

func TestReorderQuestions(t *testing.T) {
	e := newQEnv(t)
	var qs []uuid.UUID
	for _, l := range []string{"A", "B", "C"} {
		q, err := e.question(qn(l, "text"))
		if err != nil {
			t.Fatal(err)
		}
		qs = append(qs, q.ID)
	}
	want := []uuid.UUID{qs[2], qs[0], qs[1]}
	list, err := e.reorderQ(want)
	if err != nil || !slices.Equal([]uuid.UUID{list[0].ID, list[1].ID, list[2].ID}, want) || list[0].Position != 1 || list[2].Position != 3 {
		t.Fatalf("reorder: %v %+v", err, list)
	}
	if _, err := e.reorderQ(want); err != nil {
		t.Fatal(err)
	}
	if n := e.auditCount(t, "questions.reordered"); n != 1 {
		t.Fatalf("audit entries: %d, want 1", n)
	}
	for name, ids := range map[string][]uuid.UUID{
		"missing":   qs[:2],
		"extra":     append(slices.Clone(qs), uuid.New()),
		"duplicate": {qs[0], qs[0], qs[1]},
		"foreign":   {qs[0], qs[1], e.foreign.ID},
	} {
		if _, err := e.reorderQ(ids); !invalidCode(err, "order_mismatch") {
			t.Errorf("%s: got %v, want order_mismatch", name, err)
		}
	}
}

// 00014 numbers the ticket types that predate it 1..n in creation order.
func TestMigration14BacksUpTicketTypePositions(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	root := testdb.Owner(t, e.pool)
	sqlDB := stdlib.OpenDBFromPool(root)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 13); err != nil {
		t.Fatal(err)
	}
	// Old data: every position 0, a second event with its own numbering.
	var other uuid.UUID
	if err := root.QueryRow(ctx, `insert into events(organizer_id, slug, name, timezone, starts_at, ends_at)
		select organizer_id, 'other', 'Other', timezone, starts_at, ends_at from events where id = $1 returning id`, e.ev.ID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Exec(ctx, `update ticket_types set position = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Exec(ctx, `insert into ticket_types(organizer_id, event_id, name, price_cents, position)
		select organizer_id, $1, 'Solo', 100, 0 from events where id = $1`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Exec(ctx, `update ticket_types set created_at = now() - interval '1 day' where id = $1`, e.vip.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 14); err != nil {
		t.Fatal(err)
	}
	rows, err := root.Query(ctx, `select name, position from ticket_types where event_id = $1 order by position`, e.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Name string
		Pos  int
	}])
	if err != nil || len(got) != 2 || got[0].Name != "VIP" || got[0].Pos != 1 || got[1].Name != "General" || got[1].Pos != 2 {
		t.Fatalf("backfill: %v %+v", err, got)
	}
	var solo int
	if err := root.QueryRow(ctx, `select position from ticket_types where event_id = $1`, other).Scan(&solo); err != nil || solo != 1 {
		t.Fatalf("second event: %v %d", err, solo)
	}
	if _, err := root.Exec(ctx, `update ticket_types set position = 0 where id = $1`, e.vip.ID); err == nil {
		t.Fatal("position 0 accepted after 00014")
	}
}
