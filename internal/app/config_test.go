package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaultsWithoutAFile(t *testing.T) {
	c, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "" || c.Threads != nil || c.AdvisoryTimeout != 180*time.Second || !c.Tests {
		t.Errorf("defaults = %+v; want no model, nil threads, 180s, tests", c)
	}
}

func TestSaveConfigRoundTripsAndWritesTheDocumentedForm(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not yet created")
	four := 4
	want := Config{Model: "S-20261010", Threads: &four, AdvisoryTimeout: 90 * time.Second, Tests: false}
	if err := SaveConfig(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != want.Model || got.Threads == nil || *got.Threads != 4 || got.AdvisoryTimeout != want.AdvisoryTimeout || got.Tests {
		t.Errorf("round trip = %+v; want %+v", got, want)
	}

	if err := SaveConfig(dir, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	const doc = "{\n  \"model\": \"\",\n  \"threads\": null,\n  \"advisory_timeout\": \"180s\",\n  \"tests\": true\n}\n"
	if string(data) != doc {
		t.Errorf("config.json =\n%s\nwant\n%s", data, doc)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries; saving must leave only config.json", len(entries))
	}
}

func TestLoadConfigKeepsDefaultsForAbsentFields(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"model": "B-20261010"}`)
	c, err := LoadConfig(dir)
	if err != nil || c.Model != "B-20261010" || c.AdvisoryTimeout != 180*time.Second || !c.Tests || c.Threads != nil {
		t.Errorf("LoadConfig = %+v, %v", c, err)
	}
}

func TestLoadConfigAllowsTrailingWhitespace(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "{\"advisory_timeout\": \"1s\"}\n \t\r\n")
	c, err := LoadConfig(dir)
	if err != nil || c.AdvisoryTimeout != time.Second {
		t.Errorf("LoadConfig = %+v, %v; want a one-second timeout", c, err)
	}
}

func TestLoadConfigReportsAMalformedFileWithItsPath(t *testing.T) {
	for name, doc := range map[string]string{
		"not JSON":         `{"model": `,
		"unknown field":    `{"modle": "S-20261010"}`,
		"bad timeout":      `{"advisory_timeout": "soon"}`,
		"zero timeout":     `{"advisory_timeout": "0s"}`,
		"threads below 1":  `{"threads": 0}`,
		"wrong type":       `{"tests": "yes"}`,
		"trailing content": `{} {}`,
		"trailing brace":   `{} }`,
		"trailing bracket": `{} ]`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, doc)
			_, err := LoadConfig(dir)
			if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "config.json")) {
				t.Errorf("error %v; want one naming %s", err, filepath.Join(dir, "config.json"))
			}
		})
	}
}

func writeConfig(t *testing.T, dir, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}
