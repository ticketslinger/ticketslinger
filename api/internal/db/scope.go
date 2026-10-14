package db

import (
	"context"

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

// WithOrganizer runs fn in a transaction scoped to one organizer the user belongs to.
func WithOrganizer(ctx context.Context, pool *pgxpool.Pool, user, org uuid.UUID, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`select set_config('app.user_id', $1, true), set_config('app.organizer_id', $2, true)`,
			user.String(), org.String()); err != nil {
			return err
		}
		return fn(tx)
	})
}
