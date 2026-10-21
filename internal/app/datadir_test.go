package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataDirPerOperatingSystem(t *testing.T) {
	home := filepath.Join(t.TempDir(), "a b")
	for _, tc := range []struct {
		name, goos string
		env        map[string]string
		want       string
	}{
		{"macOS", "darwin", nil, filepath.Join(home, "Library", "Application Support", "stave")},
		{"Linux", "linux", nil, filepath.Join(home, ".local", "share", "stave")},
		{"Linux with XDG_DATA_HOME", "linux", map[string]string{"XDG_DATA_HOME": filepath.Join(home, "data")}, filepath.Join(home, "data", "stave")},
		{"Linux ignores a relative XDG_DATA_HOME", "linux", map[string]string{"XDG_DATA_HOME": "rel"}, filepath.Join(home, ".local", "share", "stave")},
		{"Windows", "windows", map[string]string{"LOCALAPPDATA": filepath.Join(home, "AppData", "Local")}, filepath.Join(home, "AppData", "Local", "stave")},
		{"STAVE_HOME overrides", "darwin", map[string]string{"STAVE_HOME": filepath.Join(home, "elsewhere")}, filepath.Join(home, "elsewhere")},
		{"STAVE_HOME overrides on Windows", "windows", map[string]string{"STAVE_HOME": filepath.Join(home, "elsewhere")}, filepath.Join(home, "elsewhere")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dataDir(tc.goos, func(k string) string { return tc.env[k] }, func() (string, error) { return home, nil })
			if err != nil || got != tc.want {
				t.Errorf("dataDir = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestDataDirMakesARelativeOverrideAbsolute(t *testing.T) {
	got, err := dataDir("linux", func(k string) string { return map[string]string{"STAVE_HOME": "rel"}[k] }, nil)
	want, _ := filepath.Abs("rel")
	if err != nil || got != want {
		t.Errorf("dataDir = %q, %v; want %q", got, err, want)
	}
}

func TestDataDirSaysHowToProceedWithoutAHome(t *testing.T) {
	noHome := func() (string, error) { return "", errors.New("$HOME is not defined") }
	none := func(string) string { return "" }
	for _, goos := range []string{"darwin", "linux", "windows"} {
		if _, err := dataDir(goos, none, noHome); err == nil || !strings.Contains(err.Error(), "STAVE_HOME") {
			t.Errorf("%s: error %v; want one naming STAVE_HOME", goos, err)
		}
	}
}
