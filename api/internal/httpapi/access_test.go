package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/db"
	"github.com/ticketslinger/ticketslinger/api/internal/organizers"
	"github.com/ticketslinger/ticketslinger/api/internal/testdb"
)

// No write operation is open to keys yet, so drive inOrganizer directly: a
// read key may read but not write; a write key may do both.
func TestKeyScopeGatesWrites(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	owner := testdb.Owner(t, pool)
	var org uuid.UUID
	if err := owner.QueryRow(ctx, `insert into organizers(name, slug) values ('Acme', 'acme') returning id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	s := &server{Deps: Deps{Pool: pool, Organizers: organizers.New(pool)}}
	for _, scope := range []string{"read", "write"} {
		var id uuid.UUID
		if err := owner.QueryRow(ctx, `insert into api_keys(organizer_id, name, key_hash, prefix, scope)
			values ($1, 'k', sha256(gen_random_uuid()::text::bytea), 'abcdefgh', $2) returning id`, org, scope).Scan(&id); err != nil {
			t.Fatal(err)
		}
		kctx := context.WithValue(ctx, authKey, authed{key: db.Principal{KeyID: id, KeyOrg: org, KeyScope: scope}})
		for _, write := range []bool{false, true} {
			ran := false
			err := s.inOrganizer(kctx, "acme", Access{Write: write}, func(o organizers.Organizer, _ pgx.Tx) error {
				ran = o.ID == org
				return nil
			})
			var ae *apiError
			if scope == "read" && write {
				if !errors.As(err, &ae) || ae.Status != 403 || ae.Code != "scope_read_only" || ran {
					t.Errorf("read key writing: err=%v ran=%v, want 403 scope_read_only", err, ran)
				}
				continue
			}
			if err != nil || !ran {
				t.Errorf("%s key, write=%v: err=%v ran=%v", scope, write, err, ran)
			}
		}
	}
}
