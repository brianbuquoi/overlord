package auth

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	restore := SetCostForTesting(bcrypt.MinCost)
	code := m.Run()
	restore()
	os.Exit(code)
}
