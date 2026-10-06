package testenv

import (
	"errors"
	"io/fs"
	"path/filepath"
	"runtime"

	"github.com/joho/godotenv"
)

func init() {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("testenv: cannot locate its own source file")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", ".env")
	if err := godotenv.Load(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		panic(err)
	}
}
