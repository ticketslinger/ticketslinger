package httpapi

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func TestBusyIs503TryAgain(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, sales.ErrBusy)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), `"try_again"`) {
		t.Fatalf("got %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}

// Spec §13.6: a suspended organizer's storefront answers 410
// event_unavailable, whether Go caught it or the sales trigger did.
func TestSuspendedIs410EventUnavailable(t *testing.T) {
	for _, err := range []error{
		sales.ErrUnavailable,
		fmt.Errorf("hold: %w", &pgconn.PgError{Code: "P0001", Message: "organizer_suspended"}),
	} {
		rec := httptest.NewRecorder()
		writeError(rec, salesError(err))
		if rec.Code != 410 || !strings.Contains(rec.Body.String(), `"event_unavailable"`) || !strings.Contains(rec.Body.String(), "This event isn't available.") {
			t.Errorf("%v: got %d %s", err, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	writeError(rec, salesError(&pgconn.PgError{Code: "P0001", Message: "something_else"}))
	if rec.Code != 500 {
		t.Errorf("another P0001: got %d, want 500", rec.Code)
	}
}
