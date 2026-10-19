package catalogue_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// qenv is org a with a draft event and two ticket types, and org b with one of its own.
type qenv struct {
	pool         *pgxpool.Pool
	user, a, b   uuid.UUID
	actor        audit.Actor
	ev           catalogue.Event
	general, vip catalogue.TicketType
	foreign      catalogue.TicketType
	run          func(org uuid.UUID, fn func(tx pgx.Tx) error) error
}

func newQEnv(t *testing.T) *qenv {
	t.Helper()
	pool := testdb.New(t)
	ctx := context.Background()
	user, orgs := seed(t, pool)
	e := &qenv{pool: pool, user: user, a: orgs[0], b: orgs[1], actor: audit.Actor{User: user}}
	e.run = func(org uuid.UUID, fn func(tx pgx.Tx) error) error { return as(t, pool, user, org, fn) }
	mk := func(org uuid.UUID, slug string) (ev catalogue.Event, g, v catalogue.TicketType) {
		if err := e.run(org, func(tx pgx.Tx) (err error) {
			if ev, err = catalogue.CreateEvent(ctx, tx, org, e.actor, in(slug)); err != nil {
				return
			}
			if g, err = catalogue.CreateTicketType(ctx, tx, org, ev.ID, e.actor, tt("General")); err != nil {
				return
			}
			v, err = catalogue.CreateTicketType(ctx, tx, org, ev.ID, e.actor, tt("VIP"))
			return
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	e.ev, e.general, e.vip = mk(e.a, "gala")
	_, e.foreign, _ = mk(e.b, "other")
	return e
}

func (e *qenv) create(t *testing.T, name string, size int, ids ...uuid.UUID) (q catalogue.Quota, err error) {
	t.Helper()
	err = e.run(e.a, func(tx pgx.Tx) (x error) {
		q, x = catalogue.CreateQuota(context.Background(), tx, e.a, e.ev.ID, e.actor, catalogue.QuotaInput{Name: name, Size: size, TicketTypeIDs: ids})
		return
	})
	return
}

func (e *qenv) patch(q uuid.UUID, p catalogue.QuotaPatch) (out catalogue.Quota, err error) {
	err = e.run(e.a, func(tx pgx.Tx) (x error) {
		out, x = catalogue.UpdateQuota(context.Background(), tx, e.a, e.ev.ID, q, e.actor, p)
		return
	})
	return
}

func (e *qenv) del(q uuid.UUID) error {
	return e.run(e.a, func(tx pgx.Tx) error {
		return catalogue.DeleteQuota(context.Background(), tx, e.a, e.ev.ID, q, e.actor)
	})
}

func (e *qenv) setStatus(t *testing.T, st string) error {
	t.Helper()
	return e.run(e.a, func(tx pgx.Tx) error {
		_, err := catalogue.UpdateEvent(context.Background(), tx, e.a, e.ev.ID, e.actor, catalogue.EventPatch{Status: &st}, catalogue.PaymentsConnect)
		return err
	})
}

func TestQuotaRejectsForeignTicketTypes(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	// B's ticket type, named by A's admin.
	if _, err := e.create(t, "Hall", 100, e.general.ID, e.foreign.ID); !invalidCode(err, "unknown_ticket_type") {
		t.Fatalf("other tenant's ticket type: %v", err)
	}
	// Another event's ticket type in the same org.
	var other catalogue.TicketType
	if err := e.run(e.a, func(tx pgx.Tx) error {
		ev2, err := catalogue.CreateEvent(ctx, tx, e.a, e.actor, in("second"))
		if err != nil {
			return err
		}
		other, err = catalogue.CreateTicketType(ctx, tx, e.a, ev2.ID, e.actor, tt("Elsewhere"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.create(t, "Hall", 100, other.ID); !invalidCode(err, "unknown_ticket_type") {
		t.Fatalf("other event's ticket type: %v", err)
	}
	q, err := e.create(t, "Hall", 100, e.general.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.patch(q.ID, catalogue.QuotaPatch{TicketTypeIDs: &[]uuid.UUID{e.foreign.ID}}); !invalidCode(err, "unknown_ticket_type") {
		t.Fatalf("patch with foreign: %v", err)
	}
	var n int
	if err := testdb.Owner(t, e.pool).QueryRow(ctx, `select count(*) from quota_ticket_types where ticket_type_id in ($1, $2)`, e.foreign.ID, other.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("links to foreign ticket types: %d %v", n, err)
	}
	// Same org, different event: the composite foreign keys refuse it even when
	// Go is bypassed. Raw SQL as the app role, under RLS.
	rawErr := func(sql string, args ...any) error {
		return e.run(e.a, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err })
	}
	err = rawErr(`insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, e.a, q.EventID, q.ID, other.ID)
	var pg *pgconn.PgError
	if !asPg(err, &pg) || pg.Code != "23503" {
		t.Fatalf("cross-event link: %v", err)
	}
	// And a cross-tenant link dies on the organizer foreign key.
	if err := rawErr(`insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id) values ($1, $2, $3, $4)`, e.a, q.EventID, q.ID, e.foreign.ID); err == nil {
		t.Fatal("cross-tenant link accepted")
	}
	// A parent can't move to another event while it has links.
	var ev2 uuid.UUID
	if err := testdb.Owner(t, e.pool).QueryRow(ctx, `select event_id from ticket_types where id = $1`, other.ID).Scan(&ev2); err != nil {
		t.Fatal(err)
	}
	err = rawErr(`update quotas set event_id = $2 where id = $1`, q.ID, ev2)
	if !asPg(err, &pg) || pg.Code != "23503" {
		t.Fatalf("move quota: %v", err)
	}
	err = rawErr(`update ticket_types set event_id = $2 where id = $1`, e.general.ID, ev2)
	if !asPg(err, &pg) || pg.Code != "23503" {
		t.Fatalf("move ticket type: %v", err)
	}
}

func asPg(err error, pg **pgconn.PgError) bool {
	for err != nil {
		if p, ok := err.(*pgconn.PgError); ok {
			*pg = p
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestQuotaCRUDAndReplace(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		size int
		code string
	}{{"", 1, "invalid_name"}, {strings.Repeat("x", 121), 1, "invalid_name"}, {"A", -1, "bad_quota_size"}, {"A", 1000001, "bad_quota_size"}} {
		if _, err := e.create(t, c.name, c.size); !invalidCode(err, c.code) {
			t.Errorf("%q %d: %v", c.name, c.size, err)
		}
	}
	// Duplicates collapse; an empty list is a capacity with nothing in it yet.
	q, err := e.create(t, "Hall", 100, e.general.ID, e.general.ID)
	if err != nil || len(q.TicketTypeIDs) != 1 {
		t.Fatalf("create: %v %+v", err, q)
	}
	empty, err := e.create(t, "Balcony", 0)
	if err != nil || len(empty.TicketTypeIDs) != 0 || empty.TicketTypeIDs == nil {
		t.Fatalf("empty: %v %+v", err, empty)
	}
	// Absent keeps the links; present replaces them.
	size := 250
	up, err := e.patch(q.ID, catalogue.QuotaPatch{Size: &size})
	if err != nil || up.Size != 250 || len(up.TicketTypeIDs) != 1 || up.TicketTypeIDs[0] != e.general.ID || !up.UpdatedAt.After(q.UpdatedAt) {
		t.Fatalf("size only: %v %+v", err, up)
	}
	both := []uuid.UUID{e.vip.ID, e.vip.ID}
	up, err = e.patch(q.ID, catalogue.QuotaPatch{TicketTypeIDs: &both})
	if err != nil || len(up.TicketTypeIDs) != 1 || up.TicketTypeIDs[0] != e.vip.ID || up.Size != 250 {
		t.Fatalf("replace: %v %+v", err, up)
	}
	none := []uuid.UUID{}
	if up, err = e.patch(q.ID, catalogue.QuotaPatch{TicketTypeIDs: &none}); err != nil || len(up.TicketTypeIDs) != 0 {
		t.Fatalf("clear: %v %+v", err, up)
	}
	// A no-op writes nothing.
	same := []uuid.UUID{}
	name := "Hall"
	noop, err := e.patch(q.ID, catalogue.QuotaPatch{Name: &name, Size: &size, TicketTypeIDs: &same})
	if err != nil || !noop.UpdatedAt.Equal(up.UpdatedAt) {
		t.Fatalf("noop: %v %+v", err, noop)
	}
	var list []catalogue.Quota
	if err := e.run(e.a, func(tx pgx.Tx) (err error) { list, err = catalogue.ListQuotas(ctx, tx, e.ev.ID); return }); err != nil || len(list) != 2 || list[0].ID != q.ID {
		t.Fatalf("list: %v %+v", err, list)
	}
	// B can't see or touch it.
	if err := e.run(e.b, func(tx pgx.Tx) error { _, err := catalogue.GetQuota(ctx, tx, e.ev.ID, q.ID); return err }); err != catalogue.ErrNotFound {
		t.Fatalf("other tenant get: %v", err)
	}
	if err := e.run(e.b, func(tx pgx.Tx) error { return catalogue.DeleteQuota(ctx, tx, e.b, e.ev.ID, q.ID, e.actor) }); err != catalogue.ErrNotFound {
		t.Fatalf("other tenant delete: %v", err)
	}
	if err := e.del(q.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.del(q.ID); err != catalogue.ErrNotFound {
		t.Fatalf("delete twice: %v", err)
	}
	// Audit: one entry per write, links in before/after, none for the no-op.
	counts := map[string]int{}
	var upAfter string
	if err := e.run(e.a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select action, coalesce(after::text, before::text) from audit_log where action like 'quota.%' order by at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a, j string
			if err := rows.Scan(&a, &j); err != nil {
				return err
			}
			counts[a]++
			if a == "quota.updated" && strings.Contains(j, e.vip.ID.String()) {
				upAfter = j
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if counts["quota.created"] != 2 || counts["quota.updated"] != 3 || counts["quota.deleted"] != 1 || !strings.Contains(upAfter, "ticket_type_ids") {
		t.Fatalf("audit: %v %q", counts, upAfter)
	}
}

func TestDeletingTicketTypeUnlinksQuotas(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	q, err := e.create(t, "Hall", 100, e.general.ID, e.vip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) error {
		return catalogue.DeleteTicketType(ctx, tx, e.a, e.ev.ID, e.general.ID, e.actor)
	}); err != nil {
		t.Fatalf("delete referenced ticket type: %v", err)
	}
	var got catalogue.Quota
	if err := e.run(e.a, func(tx pgx.Tx) (err error) { got, err = catalogue.GetQuota(ctx, tx, e.ev.ID, q.ID); return }); err != nil {
		t.Fatal(err)
	}
	if len(got.TicketTypeIDs) != 1 || got.TicketTypeIDs[0] != e.vip.ID {
		t.Fatalf("links after delete: %+v", got.TicketTypeIDs)
	}
}

func TestGoLiveNeedsEveryTicketTypeInAQuota(t *testing.T) {
	e := newQEnv(t)
	if err := e.setStatus(t, "live"); !invalidCode(err, "unquotaed_ticket_type") {
		t.Fatalf("no quotas: %v", err)
	} else if ie := err.(catalogue.ErrInvalid); !strings.Contains(ie.Message, "“General”") {
		t.Fatalf("message should name the first ticket type: %q", ie.Message)
	}
	q, err := e.create(t, "Hall", 100, e.general.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = e.setStatus(t, "live")
	if ie, ok := err.(catalogue.ErrInvalid); !ok || ie.Code != "unquotaed_ticket_type" || !strings.Contains(ie.Message, "“VIP”") {
		t.Fatalf("one left out: %v", err)
	}
	both := []uuid.UUID{e.general.ID, e.vip.ID}
	if _, err := e.patch(q.ID, catalogue.QuotaPatch{TicketTypeIDs: &both}); err != nil {
		t.Fatal(err)
	}
	if err := e.setStatus(t, "live"); err != nil {
		t.Fatalf("all covered: %v", err)
	}
}

func TestLiveEventKeepsEveryTicketTypeInAQuota(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	q1, err := e.create(t, "Hall", 100, e.general.ID, e.vip.ID)
	if err != nil {
		t.Fatal(err)
	}
	q2, err := e.create(t, "Front", 10, e.vip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.setStatus(t, "live"); err != nil {
		t.Fatal(err)
	}
	onlyVIP := []uuid.UUID{e.vip.ID}
	if _, err := e.patch(q1.ID, catalogue.QuotaPatch{TicketTypeIDs: &onlyVIP}); !conflict(err, "live_needs_capacity") {
		t.Fatalf("unlink only quota: %v", err)
	}
	if err := e.del(q1.ID); !conflict(err, "live_needs_capacity") {
		t.Fatalf("delete only quota: %v", err)
	}
	// Rejected writes leave everything as it was.
	var got catalogue.Quota
	if err := e.run(e.a, func(tx pgx.Tx) (err error) { got, err = catalogue.GetQuota(ctx, tx, e.ev.ID, q1.ID); return }); err != nil || len(got.TicketTypeIDs) != 2 {
		t.Fatalf("after refusals: %v %+v", err, got)
	}
	// VIP also sits in Front, so it can leave Hall; then Front is VIP's only home.
	if _, err := e.patch(q1.ID, catalogue.QuotaPatch{TicketTypeIDs: &[]uuid.UUID{e.general.ID}}); err != nil {
		t.Fatalf("unlink covered ticket type: %v", err)
	}
	if err := e.del(q2.ID); !conflict(err, "live_needs_capacity") {
		t.Fatalf("delete VIP's last quota: %v", err)
	}
	size := 5
	if _, err := e.patch(q2.ID, catalogue.QuotaPatch{Size: &size}); err != nil {
		t.Fatalf("resize on live: %v", err)
	}
	// Back in draft the same delete is fine.
	if err := e.setStatus(t, "draft"); err != nil {
		t.Fatal(err)
	}
	if err := e.del(q2.ID); err != nil {
		t.Fatalf("draft delete: %v", err)
	}
}

func TestTicketTypeCreateJoinsQuotas(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	q, err := e.create(t, "Hall", 100, e.general.ID, e.vip.ID)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(org uuid.UUID, name string, quotas ...uuid.UUID) (x catalogue.TicketType, err error) {
		in := tt(name)
		in.QuotaIDs = quotas
		err = e.run(org, func(tx pgx.Tx) (z error) {
			x, z = catalogue.CreateTicketType(ctx, tx, org, e.ev.ID, e.actor, in)
			return
		})
		return
	}
	// A foreign quota is 422 whoever holds it: B's own, or none at all.
	var bq catalogue.Quota
	if err := e.run(e.b, func(tx pgx.Tx) error {
		ev, err := catalogue.CreateEvent(ctx, tx, e.b, e.actor, in("b-gala"))
		if err != nil {
			return err
		}
		bq, err = catalogue.CreateQuota(ctx, tx, e.b, ev.ID, e.actor, catalogue.QuotaInput{Name: "B", Size: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mk(e.a, "Early", bq.ID); !invalidCode(err, "unknown_quota") {
		t.Fatalf("other tenant's quota: %v", err)
	}
	// Draft: optional, linked when given.
	linked, err := mk(e.a, "Linked", q.ID, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mk(e.a, "Loose"); err != nil {
		t.Fatalf("draft without quotas: %v", err)
	}
	var got catalogue.Quota
	if err := e.run(e.a, func(tx pgx.Tx) (err error) { got, err = catalogue.GetQuota(ctx, tx, e.ev.ID, q.ID); return }); err != nil || len(got.TicketTypeIDs) != 3 {
		t.Fatalf("linked: %v %+v (want %v among 3)", err, got.TicketTypeIDs, linked.ID)
	}
	// Another event's quota in the same org.
	var other catalogue.Quota
	if err := e.run(e.a, func(tx pgx.Tx) error {
		ev2, err := catalogue.CreateEvent(ctx, tx, e.a, e.actor, in("second"))
		if err != nil {
			return err
		}
		other, err = catalogue.CreateQuota(ctx, tx, e.a, ev2.ID, e.actor, catalogue.QuotaInput{Name: "Elsewhere", Size: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mk(e.a, "Early", other.ID); !invalidCode(err, "unknown_quota") {
		t.Fatalf("other event's quota: %v", err)
	}
	// Live: quota_ids is mandatory. (Cover the two ticket types left bare first.)
	all := []uuid.UUID{e.general.ID, e.vip.ID, linked.ID}
	var loose catalogue.TicketType
	if err := e.run(e.a, func(tx pgx.Tx) error {
		ts, err := catalogue.ListTicketTypes(ctx, tx, e.ev.ID)
		for _, x := range ts {
			if x.Name == "Loose" {
				loose = x
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	all = append(all, loose.ID)
	if _, err := e.patch(q.ID, catalogue.QuotaPatch{TicketTypeIDs: &all}); err != nil {
		t.Fatal(err)
	}
	if err := e.setStatus(t, "live"); err != nil {
		t.Fatal(err)
	}
	if _, err := mk(e.a, "Late"); !conflict(err, "live_needs_capacity") {
		t.Fatalf("live without quota_ids: %v", err)
	}
	if _, err := mk(e.a, "Late", other.ID); !invalidCode(err, "unknown_quota") {
		t.Fatalf("live with foreign quota: %v", err)
	}
	late, err := mk(e.a, "Late", q.ID)
	if err != nil {
		t.Fatalf("live with a quota: %v", err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) (err error) { got, err = catalogue.GetQuota(ctx, tx, e.ev.ID, q.ID); return }); err != nil || !slices.Contains(got.TicketTypeIDs, late.ID) {
		t.Fatalf("late not linked: %v %+v", err, got.TicketTypeIDs)
	}
	// The audit payload carries quota_ids.
	var j string
	if err := e.run(e.a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select after::text from audit_log where action = 'ticket_type.created' and entity_id = $1`, late.ID).Scan(&j)
	}); err != nil || !strings.Contains(j, `"quota_ids"`) || !strings.Contains(j, q.ID.String()) {
		t.Fatalf("audit: %v %s", err, j)
	}
}
