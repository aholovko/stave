// Package app shares installation paths, configuration and version information across commands.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Version is set with -ldflags "-X github.com/aholovko/stave/internal/app.Version=<tag>".
// It also selects the matching runtime directory.
var Version = "dev"

// DataDir returns the directory that holds config.json, runtime/, models/ and downloads/. It is STAVE_HOME when set,
// and otherwise the per-user data directory of the platform.
func DataDir() (string, error) {
	return dataDir(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

func dataDir(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if dir := getenv("STAVE_HOME"); dir != "" {
		return filepath.Abs(dir)
	}
	if goos == "windows" {
		if dir := getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, "stave"), nil
		}
		return "", errors.New("LOCALAPPDATA is not set; set STAVE_HOME to the data directory")
	}
	if dir := getenv("XDG_DATA_HOME"); goos != "darwin" && filepath.IsAbs(dir) {
		return filepath.Join(dir, "stave"), nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("no home directory (%v); set STAVE_HOME to the data directory", err)
	}
	if goos == "darwin" {
		return filepath.Join(h, "Library", "Application Support", "stave"), nil
	}
	return filepath.Join(h, ".local", "share", "stave"), nil
}
