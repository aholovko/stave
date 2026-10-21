// Package models validates, installs and selects model bundles in the data directory.
package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aholovko/stave/internal/app"
)

// Format is the bundle format this binary reads.
const Format = "stave-model-bundle-1"

// ManifestFile is the name of the manifest at the top of a bundle.
const ManifestFile = "manifest.json"

// Tasks are the six tasks that a bundle serves, in their order.
var Tasks = []string{"P1", "P2", "P3", "TF", "DC", "RP"}

// modelFormats maps each model format that the runtime supports to the variants it carries.
var modelFormats = map[string][]string{
	"stave-flat-1":       {"B"},
	"stave-structural-6": {"F", "S"},
}

var tokenizerFiles = []string{"vocab.json", "merges.txt", "tokenizer_config.json", "special_tokens_map.json", "config.json"}

// Bundle IDs become directory names under models/.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Manifest contains the installation and selection metadata. The runtime's check-bundle command verifies full model
// compatibility.
type Manifest struct {
	Format       string             `json:"format"`
	ID           string             `json:"id"`
	Variant      string             `json:"variant"`
	Version      string             `json:"version"`
	Description  string             `json:"description"`
	Tasks        []string           `json:"tasks"`
	Seed         int64              `json:"seed"`
	Thresholds   map[string]float64 `json:"thresholds"`
	BatchSize    int                `json:"batch_size"`
	InputBudgets map[string]int     `json:"input_budgets"`
	Model        ModelFile          `json:"model"`
	Backbone     Backbone           `json:"backbone"`
	Tokenizer    Tokenizer          `json:"tokenizer"`
	Provenance   map[string]any     `json:"provenance"`
	License      string             `json:"license"`
}

// ModelFile is the model section of the manifest.
type ModelFile struct {
	File   string `json:"file"`
	Format string `json:"format"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Backbone is the backbone identity in the manifest.
type Backbone struct {
	Name         string `json:"name"`
	Revision     string `json:"revision"`
	Config       string `json:"config"`
	ConfigSHA256 string `json:"config_sha256"`
	WeightSHA256 string `json:"weight_sha256"`
	WeightBytes  int64  `json:"weight_bytes"`
	Parameters   int64  `json:"parameters"`
}

// Tokenizer is the tokenizer receipt in the manifest.
type Tokenizer struct {
	Dir      string            `json:"dir"`
	Revision string            `json:"revision"`
	Files    map[string]string `json:"files"`
}

// ReadManifest reads and checks dir/manifest.json. A refusal names the first manifest field that this binary cannot
// accept.
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("manifest %s: %v", filepath.Join(dir, ManifestFile), err)
	}
	return m, m.check()
}

func (m Manifest) check() error {
	if m.Format != Format {
		return fmt.Errorf("manifest format: %q is not %s", m.Format, Format)
	}
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("manifest id: %q is not a name of letters, digits, '.', '_' and '-'", m.ID)
	}
	variants, ok := modelFormats[m.Model.Format]
	if !ok {
		return fmt.Errorf("manifest model.format: %q is not one of %s; a new model format needs a new STAVE release",
			m.Model.Format, strings.Join(slices.Sorted(maps.Keys(modelFormats)), ", "))
	}
	if !slices.Contains(variants, m.Variant) {
		return fmt.Errorf("manifest variant: %q is not one of %s for %s", m.Variant, strings.Join(variants, ", "), m.Model.Format)
	}
	if !slices.Equal(m.Tasks, Tasks) {
		return fmt.Errorf("manifest tasks: %q are not the six tasks %q in order", m.Tasks, Tasks)
	}
	for field, name := range map[string]string{"model.file": m.Model.File, "backbone.config": m.Backbone.Config, "tokenizer.dir": m.Tokenizer.Dir} {
		if !inside(name) {
			return fmt.Errorf("manifest %s: %q is not a relative path inside the bundle", field, name)
		}
	}
	if names := slices.Sorted(maps.Keys(m.Tokenizer.Files)); !slices.Equal(names, slices.Sorted(slices.Values(tokenizerFiles))) {
		return fmt.Errorf("manifest tokenizer.files: %q are not the files %q", names, tokenizerFiles)
	}
	return nil
}

// inside reports whether a manifest file name is a slash-separated relative path that stays inside the bundle.
func inside(name string) bool {
	if name == "" || strings.ContainsAny(name, `\:`) || path.IsAbs(name) {
		return false
	}
	return !slices.ContainsFunc(strings.Split(name, "/"), func(p string) bool { return p == "." || p == ".." || p == "" })
}

// VerifyBundle checks pinned files and rejects unpinned tokenizer files that could alter inference.
func VerifyBundle(dir string) error {
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	if err := verifyFile(filepath.Join(dir, filepath.FromSlash(m.Model.File)), m.Model.SHA256, m.Model.Bytes, "model.sha256"); err != nil {
		return err
	}
	if err := verifyFile(filepath.Join(dir, filepath.FromSlash(m.Backbone.Config)), m.Backbone.ConfigSHA256, -1, "backbone.config_sha256"); err != nil {
		return err
	}
	// Tokenizer sidecars can change behavior, so allow only pinned files and harmless file-browser metadata.
	list, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(m.Tokenizer.Dir)))
	if err != nil {
		return fmt.Errorf("manifest tokenizer.dir: %v", err)
	}
	var extra []string
	for _, e := range list {
		if !slices.Contains(tokenizerFiles, e.Name()) && !slices.Contains(app.BrowserFiles, e.Name()) {
			extra = append(extra, e.Name())
		}
	}
	if len(extra) > 0 {
		return fmt.Errorf("manifest tokenizer.dir: holds unpinned entries: %s", strings.Join(extra, ", "))
	}
	for _, name := range tokenizerFiles {
		path := filepath.Join(dir, filepath.FromSlash(m.Tokenizer.Dir), name)
		if err := verifyFile(path, m.Tokenizer.Files[name], -1, fmt.Sprintf("tokenizer.files[%q]", name)); err != nil {
			return err
		}
	}
	return nil
}

// verifyFile checks one pinned file. A size of -1 pins no size.
func verifyFile(path, want string, size int64, field string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("manifest %s: %v", field, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return fmt.Errorf("manifest %s: %v", field, err)
	}
	if size >= 0 && n != size {
		return fmt.Errorf("manifest %s: %s has %d bytes, not %d", strings.Replace(field, ".sha256", ".bytes", 1), path, n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("manifest %s: %s has sha256 %s, not %s", field, path, got, want)
	}
	return nil
}
