package apikeys_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ticketslinger/ticketslinger/api/internal/apikeys"
	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

func seed(t *testing.T, pool *pgxpool.Pool) (user, org uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `insert into users(email, name) values ('a@example.com', 'T') returning id`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := db.WithUser(ctx, pool, user, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select create_organizer('Org', 'org-a')`).Scan(&org)
	}); err != nil {
		t.Fatal(err)
	}
	return user, org
}

func create(t *testing.T, s *apikeys.Service, pool *pgxpool.Pool, user, org uuid.UUID, scope string) (apikeys.Key, string) {
	t.Helper()
	var k apikeys.Key
	var plain string
	err := db.WithOrganizer(context.Background(), pool, user, org, func(tx pgx.Tx, _ string) error {
		var err error
		k, plain, err = s.Create(context.Background(), tx, org, user, "CI", scope)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return k, plain
}

func TestCreateStoresOnlyTheHash(t *testing.T) {
	pool := testdb.New(t)
	s := apikeys.New(pool)
	user, org := seed(t, pool)
	k, plain := create(t, s, pool, user, org, "write")
	if !strings.HasPrefix(plain, "tsk_") || len(plain) < 36 {
		t.Fatalf("plaintext %q: want tsk_ and at least 36 chars", plain)
	}
	if k.Prefix != plain[4:12] || k.Scope != "write" || k.Name != "CI" {
		t.Fatalf("key %+v for %q", k, plain)
	}
	var hash []byte
	var action string
	err := testdb.Owner(t, pool).QueryRow(context.Background(),
		`select k.key_hash, a.action from api_keys k join audit_log a on a.entity_id = k.id and a.actor_user_id = $2 where k.id = $1`,
		k.ID, user).Scan(&hash, &action)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256([]byte(plain)); string(hash) != string(sum[:]) {
		t.Fatal("stored key_hash is not the SHA-256 of the plaintext")
	}
	if action != "api_key.created" {
		t.Fatalf("audit action %q", action)
	}
	var leaked int
	if err := testdb.Owner(t, pool).QueryRow(context.Background(),
		`select count(*) from api_keys where position(convert_to($1, 'UTF8') in key_hash) > 0 or prefix = $1`, plain).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("plaintext found in api_keys: n=%d err=%v", leaked, err)
	}
}

func TestCreateValidates(t *testing.T) {
	pool := testdb.New(t)
	s := apikeys.New(pool)
	user, org := seed(t, pool)
	for _, c := range []struct {
		name, scope string
		want        error
	}{
		{"", "read", apikeys.ErrInvalidName},
		{strings.Repeat("n", 81), "read", apikeys.ErrInvalidName},
		{"ok", "admin", apikeys.ErrInvalidScope},
	} {
		err := db.WithOrganizer(context.Background(), pool, user, org, func(tx pgx.Tx, _ string) error {
			_, _, err := s.Create(context.Background(), tx, org, user, c.name, c.scope)
			return err
		})
		if !errors.Is(err, c.want) {
			t.Errorf("Create(%q, %q): err=%v, want %v", c.name, c.scope, err, c.want)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	pool := testdb.New(t)
	s := apikeys.New(pool)
	ctx := context.Background()
	user, org := seed(t, pool)
	k, plain := create(t, s, pool, user, org, "read")

	p, err := s.Authenticate(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if p != (db.Principal{KeyID: k.ID, KeyOrg: org, KeyScope: "read"}) {
		t.Fatalf("principal %+v", p)
	}
	var used *time.Time
	if err := testdb.Owner(t, pool).QueryRow(ctx, `select last_used_at from api_keys where id = $1`, k.ID).Scan(&used); err != nil || used == nil {
		t.Fatalf("last_used_at=%v err=%v, want set", used, err)
	}
	// Within the minute, a second use leaves last_used_at alone.
	if _, err := s.Authenticate(ctx, plain); err != nil {
		t.Fatal(err)
	}
	var again *time.Time
	_ = testdb.Owner(t, pool).QueryRow(ctx, `select last_used_at from api_keys where id = $1`, k.ID).Scan(&again)
	if !again.Equal(*used) {
		t.Fatalf("last_used_at moved within a minute: %v -> %v", used, again)
	}

	// Over a minute later, the next use moves it.
	if _, err := testdb.Owner(t, pool).Exec(ctx, `update api_keys set last_used_at = now() - interval '2 minutes' where id = $1`, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, plain); err != nil {
		t.Fatal(err)
	}
	var later *time.Time
	_ = testdb.Owner(t, pool).QueryRow(ctx, `select last_used_at from api_keys where id = $1`, k.ID).Scan(&later)
	if later == nil || time.Since(*later) > 30*time.Second {
		t.Fatalf("last_used_at after a stale use: %v, want about now", later)
	}

	for _, bad := range []string{"", "tsk_", plain + "x", "tsk_" + strings.Repeat("A", 43), "not-a-key"} {
		if _, err := s.Authenticate(ctx, bad); !errors.Is(err, apikeys.ErrBadKey) {
			t.Errorf("Authenticate(%q): err=%v, want ErrBadKey", bad, err)
		}
	}

	err = db.WithOrganizer(ctx, pool, user, org, func(tx pgx.Tx, _ string) error {
		return s.Revoke(ctx, tx, org, user, k.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, plain); !errors.Is(err, apikeys.ErrBadKey) {
		t.Fatalf("revoked key: err=%v, want ErrBadKey", err)
	}
	var keys []apikeys.Key
	err = db.WithOrganizer(ctx, pool, user, org, func(tx pgx.Tx, _ string) error {
		keys, err = s.List(ctx, tx)
		return err
	})
	if err != nil || len(keys) != 1 || keys[0].RevokedAt == nil || keys[0].LastUsedAt == nil {
		t.Fatalf("list=%+v err=%v, want the one key, revoked and used", keys, err)
	}
	err = db.WithOrganizer(ctx, pool, user, org, func(tx pgx.Tx, _ string) error {
		return s.Revoke(ctx, tx, org, user, uuid.New())
	})
	if !errors.Is(err, apikeys.ErrNotFound) {
		t.Fatalf("revoking an unknown key: err=%v, want ErrNotFound", err)
	}
}

// lookup_api_key must never hand back the hash, whatever ts_app asks.
func TestLookupNeverReturnsTheHash(t *testing.T) {
	pool := testdb.New(t)
	user, org := seed(t, pool)
	_, plain := create(t, apikeys.New(pool), pool, user, org, "read")
	sum := sha256.Sum256([]byte(plain))
	rows, err := pool.Query(context.Background(), `select * from lookup_api_key($1)`, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for _, f := range rows.FieldDescriptions() {
		cols = append(cols, f.Name)
	}
	if strings.Join(cols, ",") != "id,organizer_id,scope" {
		t.Fatalf("lookup_api_key columns %v", cols)
	}
}
