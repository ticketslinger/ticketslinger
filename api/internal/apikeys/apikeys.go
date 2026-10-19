// Package apikeys issues, lists, revokes and checks organizer API keys.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
)

var (
	ErrBadKey       = errors.New("apikeys: unknown or revoked key")
	ErrNotFound     = errors.New("apikeys: no such key")
	ErrInvalidName  = errors.New("apikeys: name must be 1 to 80 characters")
	ErrInvalidScope = errors.New("apikeys: scope must be read or write")
)

// Prefix starts every key, so the API can tell a key from a session token.
const Prefix = "tsk_"

type Key struct {
	ID                    uuid.UUID
	Name, Prefix, Scope   string
	CreatedAt             time.Time
	LastUsedAt, RevokedAt *time.Time
}

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool} }

func hash(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

// Create issues a key for org, records api_key.created by actor, and returns
// the plaintext. Only its SHA-256 is stored; the plaintext is never seen again.
// tx must be scoped to org. It holds actor's membership until tx ends, so
// a removal under way at the same time either revokes this key too or wins
// first, and then Create returns db.ErrNotMember.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, org, actor uuid.UUID, name, scope string) (Key, string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 80 {
		return Key{}, "", ErrInvalidName
	}
	if scope != "read" && scope != "write" {
		return Key{}, "", ErrInvalidScope
	}
	var member bool
	if err := tx.QueryRow(ctx, `select hold_membership()`).Scan(&member); err != nil {
		return Key{}, "", err
	}
	if !member {
		return Key{}, "", db.ErrNotMember
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails (crypto/rand panics instead)
	enc := base64.RawURLEncoding.EncodeToString(b)
	plain := Prefix + enc
	k := Key{Name: name, Prefix: enc[:8], Scope: scope}
	err := tx.QueryRow(ctx, `insert into api_keys(organizer_id, name, key_hash, prefix, scope, created_by)
		values ($1, $2, $3, $4, $5, $6) returning id, created_at`,
		org, name, hash(plain), k.Prefix, scope, actor).Scan(&k.ID, &k.CreatedAt)
	if err != nil {
		return Key{}, "", err
	}
	err = audit.Write(ctx, tx, audit.Entry{
		Organizer: org, Actor: audit.Actor{User: actor}, Action: "api_key.created",
		EntityType: "api_key", EntityID: k.ID,
		After: map[string]string{"name": name, "prefix": k.Prefix, "scope": scope},
	})
	return k, plain, err
}

// List returns the scoped organizer's keys, revoked ones included, oldest first.
func (s *Service) List(ctx context.Context, tx pgx.Tx) ([]Key, error) {
	rows, err := tx.Query(ctx, `select id, name, prefix, scope, created_at, last_used_at, revoked_at
		from api_keys order by id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Key])
}

// Revoke stops the key working from the next statement on and records
// api_key.revoked by actor. Revoking a revoked key changes nothing;
// ErrNotFound if the scoped organizer has no such key.
func (s *Service) Revoke(ctx context.Context, tx pgx.Tx, org, actor, id uuid.UUID) error {
	var name, prefix, scope string
	err := tx.QueryRow(ctx, `update api_keys set revoked_at = now() where id = $1 and revoked_at is null
		returning name, prefix, scope`, id).Scan(&name, &prefix, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from api_keys where id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return nil
	}
	if err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{
		Organizer: org, Actor: audit.Actor{User: actor}, Action: "api_key.revoked",
		EntityType: "api_key", EntityID: id,
		Before: map[string]string{"name": name, "prefix": prefix, "scope": scope},
	})
}

// Authenticate turns a presented key into its principal and notes its use.
// The token is always hashed before the lookup, so timing says nothing about
// how much of it matched.
func (s *Service) Authenticate(ctx context.Context, plain string) (db.Principal, error) {
	h := hash(plain)
	p := db.Principal{}
	err := s.pool.QueryRow(ctx, `select id, organizer_id, scope from lookup_api_key($1)`, h).Scan(&p.KeyID, &p.KeyOrg, &p.KeyScope)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Principal{}, ErrBadKey
	}
	if err != nil {
		return db.Principal{}, err
	}
	if _, err := s.pool.Exec(ctx, `select touch_api_key($1)`, p.KeyID); err != nil {
		return db.Principal{}, err
	}
	return p, nil
}
