package sales

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

// Note is a team note on an order. Plain text: it's stored and returned
// as written, and never shown to the buyer.
type Note struct {
	ID         uuid.UUID
	AuthorID   *uuid.UUID
	AuthorName string // directory name, else email, else "Former member"
	Body       string
	CreatedAt  time.Time
}

// ErrNotYourNote is 403 not_your_note.
var ErrNotYourNote = errors.New("sales: not the note's author or an owner")

const noteName = `coalesce(nullif(d.name, ''), d.email::text, 'Former member')`

func orderOfEvent(ctx context.Context, tx pgx.Tx, event, order uuid.UUID) error {
	var one int
	err := tx.QueryRow(ctx, `select 1 from orders where id = $1 and event_id = $2`, order, event).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return catalogue.ErrNotFound
	}
	return err
}

// ListNotes returns the order's live notes, newest first. Runs as ts_app;
// the notes policy lets owners and admins of the organizer read.
func ListNotes(ctx context.Context, tx pgx.Tx, event, order uuid.UUID) ([]Note, error) {
	if err := orderOfEvent(ctx, tx, event, order); err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `select n.id, n.author, `+noteName+`, n.body, n.created_at from order_notes n
		left join app_member_directory() d on d.user_id = n.author
		where n.order_id = $1 and n.deleted_at is null order by n.created_at desc, n.id desc`, order)
	out := []Note{}
	var n Note
	if _, err := pgx.ForEachRow(rows, []any{&n.ID, &n.AuthorID, &n.AuthorName, &n.Body, &n.CreatedAt}, func() error {
		out = append(out, n)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// AddNote writes a note on the order as the actor, who must be an owner or
// admin (the policy checks). The audit row names the note, never its text.
func AddNote(ctx context.Context, tx pgx.Tx, org, event, order uuid.UUID, body string, actor audit.Actor) (Note, error) {
	body, ok := cleanText(body, 2000)
	if !ok || body == "" {
		return Note{}, errBadNoteBody
	}
	if err := orderOfEvent(ctx, tx, event, order); err != nil {
		return Note{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Note{}, err
	}
	n := Note{ID: id, AuthorID: &actor.User, Body: body}
	if _, err := tx.Exec(ctx, `insert into order_notes (id, organizer_id, order_id, author, body) values ($1, $2, $3, $4, $5)`,
		id, org, order, actor.User, body); err != nil {
		return Note{}, err
	}
	if err := tx.QueryRow(ctx, `select n.created_at, `+noteName+` from order_notes n
		left join app_member_directory() d on d.user_id = n.author where n.id = $1`, id).Scan(&n.CreatedAt, &n.AuthorName); err != nil {
		return Note{}, err
	}
	err = audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "order.note_added",
		EntityType: "order", EntityID: order, After: map[string]string{"note_id": id.String()}})
	return n, err
}

// DeleteNote clears a live note of this order: the body goes and deleted_at
// is stamped. Its author may, and so may an owner; another admin may not.
func DeleteNote(ctx context.Context, tx pgx.Tx, org, event, order, note uuid.UUID, role string, actor audit.Actor) error {
	var author *uuid.UUID
	err := tx.QueryRow(ctx, `select n.author from order_notes n join orders o on o.id = n.order_id
		where n.id = $1 and n.order_id = $2 and o.event_id = $3 and n.deleted_at is null`, note, order, event).Scan(&author)
	if errors.Is(err, pgx.ErrNoRows) {
		return catalogue.ErrNotFound
	}
	if err != nil {
		return err
	}
	if role != "owner" && (author == nil || *author != actor.User) {
		return ErrNotYourNote
	}
	tag, err := tx.Exec(ctx, `update order_notes set body = '', deleted_at = now() where id = $1 and deleted_at is null`, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return catalogue.ErrNotFound
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "order.note_deleted",
		EntityType: "order", EntityID: order, After: map[string]string{"note_id": note.String()}})
}
