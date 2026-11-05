package sales_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func (f fixture) asMember(s *sales.Service, user uuid.UUID, fn func(tx pgx.Tx, role string) error) error {
	return db.WithOrganizer(context.Background(), s.Pool, user, f.org, fn)
}

func (f fixture) addNote(s *sales.Service, user uuid.UUID, order uuid.UUID, body string) (n sales.Note, err error) {
	err = f.asMember(s, user, func(tx pgx.Tx, _ string) (err error) {
		n, err = sales.AddNote(context.Background(), tx, f.org, f.event, order, body, audit.Actor{User: user})
		return
	})
	return
}

func (f fixture) notes(s *sales.Service, user, order uuid.UUID) (out []sales.Note, err error) {
	err = f.asMember(s, user, func(tx pgx.Tx, _ string) (err error) {
		out, err = sales.ListNotes(context.Background(), tx, f.event, order)
		return
	})
	return
}

func (f fixture) deleteNote(s *sales.Service, user, order, note uuid.UUID) error {
	return f.asMember(s, user, func(tx pgx.Tx, role string) error {
		return sales.DeleteNote(context.Background(), tx, f.org, f.event, order, note, role, audit.Actor{User: user})
	})
}

func TestNotes(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	admin := f.id(t, `insert into users(email, name) values ('admin@example.com', 'Dana') returning id`)
	door := f.id(t, `insert into users(email, name) values ('door@example.com', 'Door') returning id`)
	f.exec(t, `insert into memberships(organizer_id, user_id, role) values ($1, $2, 'admin'), ($1, $3, 'checkin')`, f.org, admin, door)
	order := f.manual(t, s, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1})).ID
	other := f.manual(t, s, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1})).ID

	mine, err := f.addNote(s, f.user, order, "Called about parking")
	f.must(t, err)
	theirs, err := f.addNote(s, admin, order, "Dana rang back")
	f.must(t, err)
	if theirs.AuthorName != "Dana" {
		t.Fatalf("author %q", theirs.AuthorName)
	}
	got, err := f.notes(s, f.user, order)
	f.must(t, err)
	if len(got) != 2 || got[0].ID != theirs.ID || got[1].ID != mine.ID || got[1].AuthorName != "A" {
		t.Fatalf("notes %+v", got)
	}
	if got, err = f.notes(s, f.user, other); err != nil || len(got) != 0 {
		t.Fatalf("other order: %+v %v", got, err)
	}
	if err := f.asMember(s, door, func(tx pgx.Tx, _ string) error {
		n, err := sales.ListNotes(context.Background(), tx, f.event, order)
		if len(n) != 0 {
			t.Errorf("check-in staff read %d notes", len(n))
		}
		return err
	}); err != nil {
		t.Logf("check-in list: %v", err)
	}
	if _, err := f.addNote(s, door, order, "sneaky"); err == nil {
		t.Fatal("a check-in member added a note")
	}

	if err := f.deleteNote(s, admin, order, mine.ID); !errors.Is(err, sales.ErrNotYourNote) {
		t.Fatalf("admin deleting owner's note: %v", err)
	}
	// An admin deletes their own note; the owner can delete the rest.
	own, err := f.addNote(s, admin, order, "Mine to remove")
	f.must(t, err)
	f.must(t, f.deleteNote(s, admin, order, own.ID))
	f.must(t, f.deleteNote(s, f.user, order, theirs.ID))
	if err := f.deleteNote(s, f.user, order, theirs.ID); !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := f.deleteNote(s, f.user, other, mine.ID); !errors.Is(err, catalogue.ErrNotFound) {
		t.Fatalf("another order's note: %v", err)
	}
	if n := f.count(t, `select count(*) from order_notes where id = $1 and body = '' and deleted_at is not null`, theirs.ID); n != 1 {
		t.Fatal("deleted note kept its body or has no deleted_at")
	}
	if got, _ = f.notes(s, f.user, order); len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("after delete %+v", got)
	}
	if n := f.count(t, `select count(*) from audit_log where action = 'order.note_deleted' and entity_type = 'order' and entity_id = $1
		and after = jsonb_build_object('note_id', $2::text)`, order, theirs.ID.String()); n != 1 {
		t.Fatalf("%d note_deleted audit rows", n)
	}
	if n := f.count(t, `select count(*) from audit_log where action like 'order.note_%' and after ? 'body'`); n != 0 {
		t.Fatalf("%d audit rows carry a body", n)
	}
	for _, body := range []string{"", "   ", strings.Repeat("a", 2001), "a\x00b"} {
		_, err := f.addNote(s, f.user, order, body)
		var inv catalogue.ErrInvalid
		if !errors.As(err, &inv) || inv.Code != "bad_note" || inv.Field != "body" {
			t.Errorf("%q: %v, want bad_note on body", body, err)
		}
	}
	if _, err := f.addNote(s, f.user, uuid.New(), "x"); !errors.Is(err, catalogue.ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
}

func TestNoteIsPlainText(t *testing.T) {
	s := newService(t)
	f := seed(t, s.Pool, seedOpts{quota: 10, price: 2500})
	order := f.manual(t, s, comp(f, sales.Item{TicketTypeID: f.general, Quantity: 1})).ID
	body := "<img src=x onerror=alert(1)>\n**bold**"
	_, err := f.addNote(s, f.user, order, body)
	f.must(t, err)
	got, err := f.notes(s, f.user, order)
	f.must(t, err)
	if len(got) != 1 || got[0].Body != body {
		t.Fatalf("notes %+v, want the body back as written", got)
	}
	// The buyer's order page, read with their link, never has it.
	s2, fake := newPaidService(t)
	f2 := seed(t, s2.Pool, seedOpts{quota: 10, price: 2500})
	paid, code, token, _ := f2.linkedOrder(t, s2, fake, 1, 1)
	_, err = f2.addNote(s2, f2.user, paid, body)
	f2.must(t, err)
	view, err := s2.Order(context.Background(), code, token)
	f2.must(t, err)
	j, _ := json.Marshal(view)
	if strings.Contains(string(j), "onerror") || strings.Contains(string(j), "bold") {
		t.Fatalf("a note reached the buyer's order page: %s", j)
	}
	msg := f.ticketEmail(t, s, order)
	if strings.Contains(msg.Text+msg.HTML, "onerror") || strings.Contains(msg.Text+msg.HTML, "**bold**") {
		t.Fatal("a note reached the buyer's email")
	}
}
