package engine_test

import (
	"os"
	"testing"

	_ "github.com/n6k-io/n6kd/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if quackHome != "" {
		_ = os.RemoveAll(quackHome)
	}
	os.Exit(code)
}
