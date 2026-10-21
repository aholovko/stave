package testenv

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SHA256 is the hex SHA-256 of data.
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Bundle writes a small bundle with pinned files and returns dir. Model bytes are arbitrary because these tests fake
// the runtime compatibility check.
func Bundle(t testing.TB, dir, id, variant string) string {
	t.Helper()
	files := map[string][]byte{
		"model.pt":             []byte("weights of " + id),
		"backbone-config.json": []byte(`{"model_type": "roberta"}`),
		"LICENSE":              []byte("Apache-2.0"),
		"NOTICE":               []byte("UniXcoder"),
	}
	pinned := map[string]string{}
	for _, name := range []string{"vocab.json", "merges.txt", "tokenizer_config.json", "special_tokens_map.json", "config.json"} {
		data := []byte("tokenizer " + name)
		files["tokenizer/"+name] = data
		pinned[name] = SHA256(data)
	}
	format := "stave-structural-6"
	if variant == "B" {
		format = "stave-flat-1"
	}
	six := map[string]any{"P1": 0.0, "P2": 0.0, "P3": 0.0, "TF": 0.0, "DC": 0.0, "RP": 0.0}
	manifest := map[string]any{
		"format": "stave-model-bundle-1", "id": id, "variant": variant, "version": "2026.10.10",
		"description": "test bundle", "tasks": []string{"P1", "P2", "P3", "TF", "DC", "RP"}, "seed": 12,
		"thresholds": six, "batch_size": 2,
		"input_budgets": map[string]int{"P1": 1023, "P2": 1023, "P3": 1023, "TF": 512, "DC": 512, "RP": 512},
		"model":         map[string]any{"file": "model.pt", "format": format, "sha256": SHA256(files["model.pt"]), "bytes": len(files["model.pt"])},
		"backbone": map[string]any{
			"name": "stave-toy-roberta", "revision": "toy", "config": "backbone-config.json",
			"config_sha256": SHA256(files["backbone-config.json"]), "weight_sha256": strings.Repeat("0", 64),
			"weight_bytes": 1, "parameters": 1,
		},
		"tokenizer":  map[string]any{"dir": "tokenizer", "revision": "toy", "files": pinned},
		"provenance": map[string]any{"run": "test"}, "license": "Apache-2.0",
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	files["manifest.json"] = data
	for name, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Zip zips the files of dir under prefix ("" for the archive root). It also adds any extra empty entries by their raw
// names.
func Zip(t testing.TB, dir, prefix string, extra ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		w, err := zw.Create(prefix + filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range extra {
		w, err := zw.CreateRaw(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
