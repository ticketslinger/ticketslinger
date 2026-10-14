package catalogue

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// No input passes the Go rules and still trips a CHECK, so pin the mapping.
func TestDBErrorBackstop(t *testing.T) {
	var inv ErrInvalid
	if err := dbError(&pgconn.PgError{Code: "23514"}); !errors.As(err, &inv) || inv.Code != "invalid_event" {
		t.Errorf("23514: %v", err)
	}
	var con ErrConflict
	if err := dbError(&pgconn.PgError{Code: "23505"}); !errors.As(err, &con) || con.Code != "slug_taken" {
		t.Errorf("23505: %v", err)
	}
	if err := ttDBError(&pgconn.PgError{Code: "23514"}); !errors.As(err, &inv) || inv.Code != "invalid_ticket_type" {
		t.Errorf("ticket 23514: %v", err)
	}
	if err := ttDBError(&pgconn.PgError{Code: "23505"}); !errors.As(err, &con) || con.Code != "ticket_type_exists" {
		t.Errorf("ticket 23505: %v", err)
	}
	if err := quotaDBError(&pgconn.PgError{Code: "23514"}); !errors.As(err, &inv) || inv.Code != "invalid_quota" {
		t.Errorf("quota 23514: %v", err)
	}
}

// A link whose ticket type vanished between the check and the insert is a
// foreign key failure: 422, not a 500.
func TestForeignKeyFailuresAreInvalid(t *testing.T) {
	for name, c := range map[string]struct {
		fn   func(error) error
		code string
	}{
		"quota":    {quotaDBError, "invalid_quota"},
		"promo":    {promoDBError, "invalid_promo"},
		"question": {questionDBError, "invalid_question"},
	} {
		var inv ErrInvalid
		if err := c.fn(&pgconn.PgError{Code: "23503"}); !errors.As(err, &inv) || inv.Code != c.code {
			t.Errorf("%s 23503: %v, want %s", name, err, c.code)
		}
	}
}
