package auth

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	LowerCostForTests()
	os.Exit(m.Run())
}
