package paths

import (
	"errors"
	"os"
	"path/filepath"
)

// ConfigDir follows XDG on every OS, macOS included, unlike os.UserConfigDir.
func ConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "n6kd"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Join(errors.New("no config directory: set XDG_CONFIG_HOME or pass --config"), err)
	}
	return filepath.Join(home, ".config", "n6kd"), nil
}
