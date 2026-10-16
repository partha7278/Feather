package web

import (
	"os"
	"testing"

	"github.com/unifers/feather/internal/store"
)

func TestMain(m *testing.M) {
	store.UseCheapPasswordHashingForTests()
	os.Exit(m.Run())
}
