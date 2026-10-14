package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrNoSecondFactor = errors.New("auth: no second factor")
	ErrOneSecondStep  = errors.New("auth: one second step at a time")
)

const (
	recoveryCount = 10
	// No 0 O 1 I L U: nothing that looks like something else when read off paper.
	recoveryAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// normRecovery upper-cases a typed code, drops spaces and hyphens, and
// re-formats it as xxxx-xxxx-xxxx. Anything of another length is returned
// stripped and will simply not match.
func normRecovery(c string) string {
	c = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(c))
	if len(c) != 12 {
		return c
	}
	return c[:4] + "-" + c[4:8] + "-" + c[8:]
}

func newRecoveryCode() (string, error) {
	var b strings.Builder
	n := big.NewInt(int64(len(recoveryAlphabet)))
	for i := 0; i < 12; i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		r, err := rand.Int(rand.Reader, n) // uniform, no modulo bias
		if err != nil {
			return "", err
		}
		b.WriteByte(recoveryAlphabet[r.Int64()])
	}
	return b.String(), nil
}

// GenerateRecoveryCodes replaces the user's codes with ten new ones and
// returns them. This is the only time they exist in the clear. The Argon2
// hashing happens before the transaction: the row lock is held only for the
// factor check and the writes.
func (s *Service) GenerateRecoveryCodes(ctx context.Context, userID uuid.UUID, mfa bool) ([]string, error) {
	if !mfa {
		return nil, ErrMFARequired
	}
	if !s.recLimit.Allow("recovery:"+userID.String(), time.Now()) {
		return nil, ErrRateLimited
	}
	codes := make([]string, recoveryCount)
	hashes := make([]string, recoveryCount)
	for i := range codes {
		c, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		if s.beforeHash != nil {
			s.beforeHash()
		}
		h, err := hashPassword(ctx, c)
		if err != nil {
			return nil, err
		}
		codes[i], hashes[i] = c, h
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Same row lock as enrolment, so removing a factor can't slip in between check and write.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
			return err
		}
		var has bool
		if err := tx.QueryRow(ctx,
			`SELECT totp_enabled_at IS NOT NULL OR EXISTS (SELECT 1 FROM passkeys WHERE user_id = $1)
			   FROM users WHERE id = $1`, userID).Scan(&has); err != nil {
			return err
		}
		if !has {
			return ErrNoSecondFactor
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1`, userID); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.Exec(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES ($1, $2)`, userID, h); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// matchRecovery returns the id of the user's unused code that matches the
// typed one, or uuid.Nil. It checks every unused code (at most ten).
func (s *Service) matchRecovery(ctx context.Context, userID uuid.UUID, typed string) (uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, code_hash FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID)
	if err != nil {
		return uuid.Nil, err
	}
	type row struct {
		id   uuid.UUID
		hash string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.hash); err != nil {
			return uuid.Nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	typed = normRecovery(typed)
	found := uuid.Nil
	for _, r := range all { // no early exit: time doesn't say which code matched
		if ok, err := checkPassword(ctx, r.hash, typed); err != nil {
			return uuid.Nil, err
		} else if ok {
			found = r.id
		}
	}
	return found, nil
}
