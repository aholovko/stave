package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ConfigFile is the name of the configuration file in the data directory.
const ConfigFile = "config.json"

// BrowserFiles permits harmless Finder and Explorer metadata in otherwise restricted directories.
var BrowserFiles = []string{".DS_Store", "Thumbs.db"}

// Config holds installation defaults, overridden by explicit command-line flags. Nil Threads uses torch's default.
type Config struct {
	Model           string
	Threads         *int
	AdvisoryTimeout time.Duration
	Tests           bool
}

// DefaultConfig allows time for a cold model load on a slow machine.
func DefaultConfig() Config {
	return Config{AdvisoryTimeout: 180 * time.Second, Tests: true}
}

// file is the wire form of Config, in the documented field order.
type file struct {
	Model           string `json:"model"`
	Threads         *int   `json:"threads"`
	AdvisoryTimeout string `json:"advisory_timeout"`
	Tests           bool   `json:"tests"`
}

// LoadConfig uses defaults for absent files or fields; malformed configuration is an error.
func LoadConfig(dir string) (Config, error) {
	path := filepath.Join(dir, ConfigFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return Config{}, err
	}
	c, err := parseConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %v", path, err)
	}
	return c, nil
}

func parseConfig(data []byte) (Config, error) {
	d := DefaultConfig()
	f := file{Model: d.Model, AdvisoryTimeout: formatDuration(d.AdvisoryTimeout), Tests: d.Tests}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Config{}, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("content after the configuration object")
	}
	timeout, err := time.ParseDuration(f.AdvisoryTimeout)
	if err != nil || timeout <= 0 {
		return Config{}, fmt.Errorf("advisory_timeout %q is not a positive duration such as \"180s\"", f.AdvisoryTimeout)
	}
	if f.Threads != nil && *f.Threads < 1 {
		return Config{}, fmt.Errorf("threads %d is not null or a positive integer", *f.Threads)
	}
	return Config{Model: f.Model, Threads: f.Threads, AdvisoryTimeout: timeout, Tests: f.Tests}, nil
}

// SaveConfig stages writes so a failed write leaves the existing configuration unchanged.
func SaveConfig(dir string, c Config) error {
	data, err := json.MarshalIndent(file{
		Model: c.Model, Threads: c.Threads, AdvisoryTimeout: formatDuration(c.AdvisoryTimeout), Tests: c.Tests,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, ConfigFile))
}

// formatDuration writes whole seconds as "180s", the documented form, and not as "3m0s" as Duration.String does.
func formatDuration(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
