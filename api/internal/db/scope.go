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

// ErrNotMember means the principal has no access to the requested organizer:
// a user without a membership, or an API key of another organizer.
var ErrNotMember = errors.New("db: principal is not a member of the organizer")

// Principal is who a request acts as: a user, or an organizer's API key.
// Exactly one of UserID and KeyID is set.
type Principal struct {
	UserID   uuid.UUID // zero for keys
	KeyID    uuid.UUID // zero for users
	KeyOrg   uuid.UUID
	KeyScope string // "read" or "write"
}

// WithOrganizer runs fn in a transaction scoped to one organizer the user
// belongs to, passing the user's role there. It returns ErrNotMember
// otherwise. The database enforces the same rule independently: app_organizer()
// is NULL without a membership.
func WithOrganizer(ctx context.Context, pool *pgxpool.Pool, user, org uuid.UUID, fn func(tx pgx.Tx, role string) error) error {
	return WithPrincipal(ctx, pool, Principal{UserID: user}, org, fn)
}

// WithPrincipal is WithOrganizer for either kind of principal. A key only
// reaches its own organizer, with role "key:read" or "key:write"; the database
// independently refuses a revoked key, another organizer, or a transaction
// that names both a user and a key.
func WithPrincipal(ctx context.Context, pool *pgxpool.Pool, p Principal, org uuid.UUID, fn func(tx pgx.Tx, role string) error) error {
	if (p.UserID == uuid.Nil) == (p.KeyID == uuid.Nil) {
		return errors.New("db: a principal is exactly one of a user or a key")
	}
	if p.KeyID != uuid.Nil {
		if p.KeyScope != "read" && p.KeyScope != "write" {
			return errors.New("db: unknown key scope " + p.KeyScope)
		}
		if p.KeyOrg != org {
			return ErrNotMember
		}
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if p.KeyID != uuid.Nil {
			if _, err := tx.Exec(ctx,
				`select set_config('app.api_key_id', $1, true), set_config('app.organizer_id', $2, true)`,
				p.KeyID.String(), org.String()); err != nil {
				return err
			}
			return fn(tx, "key:"+p.KeyScope)
		}
		if _, err := tx.Exec(ctx,
			`select set_config('app.user_id', $1, true), set_config('app.organizer_id', $2, true)`,
			p.UserID.String(), org.String()); err != nil {
			return err
		}
		var role string
		err := tx.QueryRow(ctx, `select role from memberships where organizer_id = $1 and user_id = $2`, org, p.UserID).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		return fn(tx, role)
	})
}
