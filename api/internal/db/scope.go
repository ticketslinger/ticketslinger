package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WithUser runs fn in a transaction where RLS sees the user's memberships.
func WithUser(ctx context.Context, pool *pgxpool.Pool, user uuid.UUID, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('app.user_id', $1, true)`, user.String()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// ErrNotMember means the user has no membership in the requested organizer.
var ErrNotMember = errors.New("db: user is not a member of the organizer")

// WithOrganizer runs fn in a transaction scoped to one organizer the user
// belongs to, passing the user's role there. It returns ErrNotMember
// otherwise. The database enforces the same rule independently: app_organizer()
// is NULL without a membership.
func WithOrganizer(ctx context.Context, pool *pgxpool.Pool, user, org uuid.UUID, fn func(tx pgx.Tx, role string) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`select set_config('app.user_id', $1, true), set_config('app.organizer_id', $2, true)`,
			user.String(), org.String()); err != nil {
			return err
		}
		var role string
		err := tx.QueryRow(ctx, `select role from memberships where organizer_id = $1 and user_id = $2`, org, user).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		return fn(tx, role)
	})
}
