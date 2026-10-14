package catalogue_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func (e *qenv) question(in catalogue.QuestionInput) (q catalogue.Question, err error) {
	err = e.run(e.a, func(tx pgx.Tx) (x error) {
		q, x = catalogue.CreateQuestion(context.Background(), tx, e.a, e.ev.ID, e.actor, in)
		return
	})
	return
}

func (e *qenv) patchQuestion(id uuid.UUID, p catalogue.QuestionPatch) (out catalogue.Question, err error) {
	err = e.run(e.a, func(tx pgx.Tx) (x error) {
		out, x = catalogue.UpdateQuestion(context.Background(), tx, e.a, e.ev.ID, id, e.actor, p)
		return
	})
	return
}

func qn(label, kind string, opts ...string) catalogue.QuestionInput {
	return catalogue.QuestionInput{Label: label, Kind: kind, Scope: "order", Options: opts}
}

func str(s string) *string { return &s }

func TestQuestionValidation(t *testing.T) {
	e := newQEnv(t)
	many := make([]string, 31)
	for i := range many {
		many[i] = strings.Repeat("x", i+1)
	}
	badScope := qn("Q", "text")
	badScope.Scope = "cart"
	for _, c := range []struct {
		in   catalogue.QuestionInput
		code string
	}{
		{qn("   ", "text"), "bad_label"},
		{qn(strings.Repeat("é", 201), "text"), "bad_label"},
		{qn("Q", "radio"), "bad_kind"},
		{badScope, "bad_scope"},
		{qn("Q", "select"), "bad_options"},
		{qn("Q", "select", "only"), "bad_options"},
		{qn("Q", "select", many...), "bad_options"},
		{qn("Q", "select", "Red", " red "), "bad_options"},
		{qn("Q", "select", "Ses", "ſes"), "bad_options"},
		{qn("Q", "select", "ΟΔΟΣ", "οδος"), "bad_options"},
		{qn("Q", "select", "ΟΔΟΣ", "οδοσ"), "bad_options"},
		{qn("Q", "select", "Red", "  "), "bad_options"},
		{qn("Q", "select", "Red", strings.Repeat("é", 121)), "bad_options"},
		{qn("Q", "text", "a", "b"), "bad_options"},
		{qn("Q", "checkbox", "a"), "bad_options"},
		{catalogue.QuestionInput{Label: "Q", Kind: "text", Scope: "order", AppliesTo: []uuid.UUID{e.vip.ID}}, "applies_to_conflict"},
		{catalogue.QuestionInput{Label: "Q", Kind: "text", Scope: "order", AppliesToAll: no()}, "applies_to_required"},
	} {
		if _, err := e.question(c.in); !invalidCode(err, c.code) {
			t.Errorf("%+v: want %s, got %v", c.in, c.code, err)
		}
	}
	if _, err := e.question(qn("Q", "select", "Red", "Red ")); err == nil || !strings.Contains(err.Error(), "bad_options") {
		t.Fatalf("duplicate option: want bad_options, got %v", err)
	} else if ie := err.(catalogue.ErrInvalid); !strings.Contains(ie.Message, "“Red”") {
		t.Fatalf("message should name the option: %q", ie.Message)
	}
	q, err := e.question(qn("  Meal?  ", "select", " Beef ", "Veg"))
	if err != nil || q.Label != "Meal?" || q.Options[0] != "Beef" || !q.AppliesToAll || q.AppliesTo == nil || q.Required {
		t.Fatalf("create: %v %+v", err, q)
	}
	if txt, err := e.question(qn("Name", "text")); err != nil || txt.Options == nil || len(txt.Options) != 0 {
		t.Fatalf("text options: %v %+v", err, txt)
	}
	// The CHECKs back the Go rules up.
	var pg *pgconn.PgError
	err = e.run(e.a, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `update questions set options = '{}'::jsonb where id = $1`, q.ID)
		return err
	})
	if !asPg(err, &pg) || pg.Code != "23514" {
		t.Fatalf("raw non-array options: %v", err)
	}
}

func TestQuestionPositionsAndPatch(t *testing.T) {
	e := newQEnv(t)
	a, _ := e.question(qn("A", "text"))
	b, _ := e.question(qn("B", "checkbox"))
	c, err := e.question(qn("C", "text"))
	if err != nil || a.Position != 1 || b.Position != 2 || c.Position != 3 {
		t.Fatalf("positions: %v %d %d %d", err, a.Position, b.Position, c.Position)
	}
	// Moving one leaves the others where they were.
	three := 3
	if _, err := e.patchQuestion(a.ID, catalogue.QuestionPatch{Position: &three}); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if _, err := e.patchQuestion(a.ID, catalogue.QuestionPatch{Position: &zero}); !invalidCode(err, "bad_position") {
		t.Fatalf("position 0: %v", err)
	}
	var list []catalogue.Question
	if err := e.run(e.a, func(tx pgx.Tx) (err error) {
		list, err = catalogue.ListQuestions(context.Background(), tx, e.ev.ID)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != b.ID || list[1].ID != a.ID || list[2].ID != c.ID || list[2].Position != 3 {
		t.Fatalf("order: %+v", list)
	}
	// A new one is max+1, even with a gap.
	d, _ := e.question(qn("D", "text"))
	if d.Position != 4 {
		t.Fatalf("after move: %d", d.Position)
	}
	// No-op writes nothing.
	same, err := e.patchQuestion(b.ID, catalogue.QuestionPatch{Label: str("B"), AppliesTo: &[]uuid.UUID{}})
	if err != nil || !same.UpdatedAt.Equal(b.UpdatedAt) {
		t.Fatalf("no-op: %v", err)
	}
	var n int
	if err := testdb.Owner(t, e.pool).QueryRow(context.Background(), `select count(*) from audit_log where action = 'question.updated'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows: %d %v", n, err)
	}
	// Kind changes.
	if _, err := e.patchQuestion(b.ID, catalogue.QuestionPatch{Kind: str("select")}); !invalidCode(err, "options_required_with_kind") {
		t.Fatalf("select without options: %v", err)
	}
	if _, err := e.patchQuestion(b.ID, catalogue.QuestionPatch{Options: &[]string{"x", "y"}}); !invalidCode(err, "bad_options") {
		t.Fatalf("options on checkbox: %v", err)
	}
	sel, err := e.patchQuestion(b.ID, catalogue.QuestionPatch{Kind: str("select"), Options: &[]string{"Yes", "No"}})
	if err != nil || sel.Kind != "select" || len(sel.Options) != 2 {
		t.Fatalf("to select: %v %+v", err, sel)
	}
	if sel, err = e.patchQuestion(b.ID, catalogue.QuestionPatch{Options: &[]string{"Yes", "yes"}}); !invalidCode(err, "bad_options") {
		t.Fatalf("dup options: %v", err)
	}
	if sel, err = e.patchQuestion(b.ID, catalogue.QuestionPatch{Kind: str("text")}); err != nil || sel.Kind != "text" || sel.Options == nil || len(sel.Options) != 0 {
		t.Fatalf("away from select: %v %+v", err, sel)
	}
	// Scope and required are plain edits; applies_to rules match promo codes.
	up, err := e.patchQuestion(b.ID, catalogue.QuestionPatch{Scope: str("ticket"), Required: yes(), AppliesToAll: no(), AppliesTo: &[]uuid.UUID{e.vip.ID, e.vip.ID}})
	if err != nil || up.Scope != "ticket" || !up.Required || up.AppliesToAll || len(up.AppliesTo) != 1 {
		t.Fatalf("edit: %v %+v", err, up)
	}
	if _, err := e.patchQuestion(b.ID, catalogue.QuestionPatch{Scope: str("cart")}); !invalidCode(err, "bad_scope") {
		t.Fatalf("bad scope: %v", err)
	}
	if _, err := e.patchQuestion(b.ID, catalogue.QuestionPatch{AppliesToAll: yes()}); !invalidCode(err, "applies_to_conflict") {
		t.Fatalf("all with list: %v", err)
	}
	if _, err := e.patchQuestion(a.ID, catalogue.QuestionPatch{AppliesToAll: no()}); !invalidCode(err, "applies_to_required") {
		t.Fatalf("limit with no list: %v", err)
	}
	up, err = e.patchQuestion(b.ID, catalogue.QuestionPatch{AppliesToAll: yes(), AppliesTo: &[]uuid.UUID{}})
	if err != nil || !up.AppliesToAll || len(up.AppliesTo) != 0 {
		t.Fatalf("widen: %v %+v", err, up)
	}
}

func TestQuestionRejectsForeignTicketTypes(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
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
	for _, id := range []uuid.UUID{e.foreign.ID, other.ID} {
		c := qn("Q", "text")
		c.AppliesToAll, c.AppliesTo = no(), []uuid.UUID{e.general.ID, id}
		if _, err := e.question(c); !invalidCode(err, "unknown_ticket_type") {
			t.Fatalf("create with %v: %v", id, err)
		}
	}
	q, err := e.question(qn("Q", "text"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{e.foreign.ID, other.ID} {
		if _, err := e.patchQuestion(q.ID, catalogue.QuestionPatch{AppliesToAll: no(), AppliesTo: &[]uuid.UUID{id}}); !invalidCode(err, "unknown_ticket_type") {
			t.Fatalf("patch with %v: %v", id, err)
		}
	}
	var n int
	if err := testdb.Owner(t, e.pool).QueryRow(ctx, `select (select count(*) from question_applies_to) + (select count(*) from questions where event_id <> $1)`, e.ev.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stray rows: %d %v", n, err)
	}
	// Bypassing Go as the app role: a cross-event link dies on the declarative FK.
	var pg *pgconn.PgError
	err = e.run(e.a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into question_applies_to(organizer_id, event_id, question_id, ticket_type_id) values ($1, $2, $3, $4)`, e.a, q.EventID, q.ID, other.ID)
		return err
	})
	if !asPg(err, &pg) || pg.Code != "23503" {
		t.Fatalf("cross-event link: %v", err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into question_applies_to(organizer_id, event_id, question_id, ticket_type_id) values ($1, $2, $3, $4)`, e.a, q.EventID, q.ID, e.foreign.ID)
		return err
	}); err == nil {
		t.Fatal("cross-tenant link accepted")
	}
}

func TestDeletingTicketTypeUnlinksQuestionsAndEventTakesThemAll(t *testing.T) {
	e := newQEnv(t)
	ctx := context.Background()
	c := qn("Seat?", "text")
	c.AppliesToAll, c.AppliesTo = no(), []uuid.UUID{e.general.ID, e.vip.ID}
	q, err := e.question(c)
	if err != nil {
		t.Fatal(err)
	}
	del := func(id uuid.UUID) {
		t.Helper()
		if err := e.run(e.a, func(tx pgx.Tx) error { return catalogue.DeleteTicketType(ctx, tx, e.a, e.ev.ID, id, e.actor) }); err != nil {
			t.Fatal(err)
		}
	}
	get := func() (got catalogue.Question) {
		if err := e.run(e.a, func(tx pgx.Tx) (err error) { got, err = catalogue.GetQuestion(ctx, tx, e.ev.ID, q.ID); return }); err != nil {
			t.Fatal(err)
		}
		return
	}
	del(e.vip.ID)
	if got := get(); len(got.AppliesTo) != 1 || got.AppliesTo[0] != e.general.ID || got.AppliesToAll {
		t.Fatalf("after one delete: %+v", got)
	}
	// Emptied: applies to nothing, never to everything; other fields still edit.
	del(e.general.ID)
	if got := get(); got.AppliesToAll || len(got.AppliesTo) != 0 {
		t.Fatalf("emptied: %+v", got)
	}
	if got, err := e.patchQuestion(q.ID, catalogue.QuestionPatch{Required: yes()}); err != nil || got.AppliesToAll || !got.Required {
		t.Fatalf("edit emptied: %v %+v", err, got)
	}
	// Audit carries options and applies_to; deleting writes one entry.
	if err := e.run(e.a, func(tx pgx.Tx) error { return catalogue.DeleteQuestion(ctx, tx, e.a, e.ev.ID, q.ID, e.actor) }); err != nil {
		t.Fatal(err)
	}
	var created, deleted int
	if err := testdb.Owner(t, e.pool).QueryRow(ctx, `select count(*) filter (where action = 'question.created' and after::text like '%applies_to%' and after::text like '%options%'),
		count(*) filter (where action = 'question.deleted' and before::text like '%options%') from audit_log`).Scan(&created, &deleted); err != nil || created != 1 || deleted != 1 {
		t.Fatalf("audit: %d %d %v", created, deleted, err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) error { _, err := catalogue.GetQuestion(ctx, tx, e.ev.ID, q.ID); return err }); err != catalogue.ErrNotFound {
		t.Fatalf("after delete: %v", err)
	}
	// Event delete removes questions and links.
	c.AppliesTo = nil
	c.AppliesToAll = yes()
	if _, err := e.question(c); err != nil {
		t.Fatal(err)
	}
	var tt2 catalogue.TicketType
	if err := e.run(e.a, func(tx pgx.Tx) (err error) {
		tt2, err = catalogue.CreateTicketType(ctx, tx, e.a, e.ev.ID, e.actor, tt("New"))
		return
	}); err != nil {
		t.Fatal(err)
	}
	c.AppliesToAll, c.AppliesTo = no(), []uuid.UUID{tt2.ID}
	if _, err := e.question(c); err != nil {
		t.Fatal(err)
	}
	if err := e.run(e.a, func(tx pgx.Tx) error { return catalogue.DeleteEvent(ctx, tx, e.a, e.ev.ID, e.actor) }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := testdb.Owner(t, e.pool).QueryRow(ctx, `select (select count(*) from questions where event_id = $1)
		+ (select count(*) from question_applies_to where event_id = $1)`, e.ev.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left after event delete: %d %v", n, err)
	}
}
