// Package organizers creates and looks up organizers (tenants).
package organizers

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
)

var (
	ErrSlugTaken = errors.New("organizers: slug taken")
	ErrNotFound  = errors.New("organizers: not found")
	ErrInvalid   = errors.New("organizers: invalid name or slug")
)

type Organizer struct {
	ID         uuid.UUID
	Name, Slug string
	Role       string
}

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool} }

// Create makes the user the owner of a new organizer and records
// organizer.created, all in one transaction.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, name, slug string) (Organizer, error) {
	o := Organizer{Name: name, Slug: slug, Role: "owner"}
	err := db.WithUser(ctx, s.pool, userID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select create_organizer($1, $2)`, name, slug).Scan(&o.ID); err != nil {
			return err
		}
		// The membership now exists, so the audit policy accepts this scope.
		if _, err := tx.Exec(ctx, `select set_config('app.organizer_id', $1, true)`, o.ID.String()); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Entry{
			Organizer: o.ID, Actor: userID, Action: "organizer.created",
			EntityType: "organizer", EntityID: o.ID,
			After: map[string]string{"name": name, "slug": slug},
		})
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return Organizer{}, ErrSlugTaken
	}
	if errors.As(err, &pg) && pg.Code == "23514" { // CHECK backstop
		return Organizer{}, ErrInvalid
	}
	if err != nil {
		return Organizer{}, err
	}
	return o, nil
}

const selectOrgs = `select o.id, o.name, o.slug::text, m.role from organizers o
	join memberships m on m.organizer_id = o.id and m.user_id = $1`

// ListForUser returns the organizers the user belongs to, oldest first.
func (s *Service) ListForUser(ctx context.Context, userID uuid.UUID) ([]Organizer, error) {
	out := []Organizer{}
	err := db.WithUser(ctx, s.pool, userID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, selectOrgs+` order by o.id`, userID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Organizer])
		return err
	})
	return out, err
}

// Resolve finds the organizer by slug; ErrNotFound if the user isn't a member.
func (s *Service) Resolve(ctx context.Context, userID uuid.UUID, slug string) (Organizer, error) {
	var o Organizer
	err := db.WithUser(ctx, s.pool, userID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, selectOrgs+` where o.slug = $2`, userID, slug).Scan(&o.ID, &o.Name, &o.Slug, &o.Role)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Organizer{}, ErrNotFound
	}
	return o, err
}
