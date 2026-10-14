package httpapi_test

import (
	"os"
	"testing"

	"github.com/ticketslinger/ticketslinger/api/internal/auth"
)

func TestMain(m *testing.M) {
	auth.LowerCostForTests()
	os.Exit(m.Run())
}
