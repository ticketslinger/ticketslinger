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
}
