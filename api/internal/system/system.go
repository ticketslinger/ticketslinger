// Package system runs transactions as the storefront (ts_public) or as the
// platform itself (ts_system). It is the only code that switches roles; the
// packages allowed to import it are listed in system_test.go.
package system

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const clear = `select set_config('app.user_id', '', true), set_config('app.api_key_id', '', true), set_config('app.organizer_id', $1, true)`

// Public runs fn read-only as ts_public: live events, their visible ticket
// types and questions, and organizer names. Nothing else.
func Public(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `set local role ts_public`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, clear, ""); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Run runs fn as ts_system for one organizer: buyers' checkouts, Stripe
// webhooks and jobs, which have no signed-in member behind them.
func Run(ctx context.Context, pool *pgxpool.Pool, org uuid.UUID, fn func(pgx.Tx) error) error {
	if org == uuid.Nil {
		return errors.New("system: a system transaction needs an organizer")
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `set local role ts_system`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, clear, org.String()); err != nil {
			return err
		}
		return fn(tx)
	})
}
