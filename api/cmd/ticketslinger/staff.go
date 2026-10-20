package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
)

// runStaff is `ticketslinger staff grant|revoke|list`. Like runMigrate it
// needs only the owner's URL: ts_app can't see the staff table, and staff are
// made only from the server's shell.
func runStaff(ctx context.Context, getenv func(string) string, args []string, out io.Writer) error {
	u := getenv("TS_MIGRATE_DATABASE_URL")
	if u == "" {
		return errors.New("TS_MIGRATE_DATABASE_URL is required for staff")
	}
	const usage = "usage: ticketslinger staff <grant|revoke> <email> | list"
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch {
	case args[0] == "list" && len(args) == 1:
	case (args[0] == "grant" || args[0] == "revoke") && len(args) == 2:
	default:
		return errors.New(usage)
	}
	var by string
	if args[0] == "grant" {
		if by = getenv("SUDO_USER"); by == "" {
			return errors.New("run this through sudo ts-staff")
		}
	}
	pool, err := db.Open(ctx, u)
	if err != nil {
		return err
	}
	defer pool.Close()
	switch args[0] {
	case "grant":
		return staffGrant(ctx, pool, out, args[1], by)
	case "revoke":
		return staffRevoke(ctx, pool, out, args[1])
	}
	return staffList(ctx, pool, out)
}

func staffGrant(ctx context.Context, p *pgxpool.Pool, out io.Writer, email, by string) error {
	var verified, passkey bool
	err := p.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL, EXISTS (SELECT 1 FROM passkeys WHERE user_id = users.id)
		FROM users WHERE email = $1`, email).Scan(&verified, &passkey)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errors.New("no account with that email")
	case err != nil:
		return err
	case !verified:
		return errors.New("that account's email isn't verified")
	case !passkey:
		return errors.New("that account has no passkey: add one in Dashboard → Security, then grant again")
	}
	if _, err := p.Exec(ctx, `INSERT INTO staff(user_id, granted_by) SELECT id, $2 FROM users WHERE email = $1
		ON CONFLICT (user_id) DO UPDATE SET revoked_at = NULL, granted_at = now(), granted_by = EXCLUDED.granted_by`, email, by); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "granted %s\n", email)
	return err
}

// staffRevoke revokes and ends every live staff session in one transaction,
// with an audit row per session on its organizer. The row names the staff
// user and session, as the sweeper's do: the CHECK on audit_log forbids the
// system actor beside a staff session.
func staffRevoke(ctx context.Context, p *pgxpool.Pool, out io.Writer, email string) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var uid string
	err = tx.QueryRow(ctx, `UPDATE staff SET revoked_at = COALESCE(revoked_at, now())
		WHERE user_id = (SELECT id FROM users WHERE email = $1) RETURNING user_id::text`, email).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("that account isn't staff")
	} else if err != nil {
		return err
	}
	var n int
	err = tx.QueryRow(ctx, `WITH ended AS (
		UPDATE staff_sessions SET ended_at = now(), ended_how = 'revoked'
		 WHERE user_id = $1 AND ended_at IS NULL RETURNING id, user_id, organizer_id),
	logged AS (
		INSERT INTO audit_log(organizer_id, actor_user_id, actor_staff_session_id, action, entity_type, entity_id, after)
		SELECT organizer_id, user_id, id, 'staff.session_ended', 'staff_session', id, '{"how": "revoked"}'
		  FROM ended WHERE organizer_id IS NOT NULL)
	SELECT count(*)::int FROM ended`, uid).Scan(&n)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "revoked %s, ended %d session(s)\n", email, n)
	return err
}

func staffList(ctx context.Context, p *pgxpool.Pool, out io.Writer) error {
	rows, err := p.Query(ctx, `SELECT u.email::text, s.granted_at, s.granted_by, s.revoked_at
		FROM staff s JOIN users u ON u.id = s.user_id ORDER BY u.email`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var email, by string
		var granted time.Time
		var revoked *time.Time
		if err := rows.Scan(&email, &granted, &by, &revoked); err != nil {
			return err
		}
		line := fmt.Sprintf("%s  %s  %s", email, granted.UTC().Format(time.RFC3339), by)
		if revoked != nil {
			line += "  " + revoked.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return rows.Err()
}
