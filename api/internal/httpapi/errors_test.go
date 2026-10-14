package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/sales"
)

func TestBusyIs503TryAgain(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, sales.ErrBusy)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), `"try_again"`) {
		t.Fatalf("got %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}
