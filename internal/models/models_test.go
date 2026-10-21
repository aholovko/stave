package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/testenv"
)

var (
	hexOf       = testenv.SHA256
	writeBundle = testenv.Bundle
	zipOf       = testenv.Zip
)

func writeFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func pass(string) error { return nil }

// checked records the directory that the compatibility test ran on.
func checked(dirs *[]string) func(string) error {
	return func(dir string) error {
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
			return err
		}
		*dirs = append(*dirs, dir)
		return nil
	}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestAddInstallsAZipWithEntriesAtTheRootOrUnderOneDirectory(t *testing.T) {
	for name, prefix := range map[string]string{"root": "", "top-level directory": "toy-S/"} {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			archive := writeFile(t, filepath.Join(t.TempDir(), "toy-S.zip"), zipOf(t, writeBundle(t, t.TempDir(), "toy-S", "S"), prefix))
			var ran []string
			m, err := Add(context.Background(), data, archive, checked(&ran), &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if m.ID != "toy-S" || m.Variant != "S" {
				t.Errorf("manifest = %s %s", m.ID, m.Variant)
			}
			if _, err := os.Stat(filepath.Join(data, "models", "toy-S", "manifest.json")); err != nil {
				t.Errorf("not installed at models/toy-S: %v", err)
			}
			if err := VerifyBundle(filepath.Join(data, "models", "toy-S")); err != nil {
				t.Errorf("installed bundle does not verify: %v", err)
			}
			if len(ran) != 1 {
				t.Errorf("compatibility test ran %d times, want once", len(ran))
			}
			if got := entries(t, filepath.Join(data, "models")); !slices.Equal(got, []string{"toy-S"}) {
				t.Errorf("models/ holds %q, want only toy-S", got)
			}
		})
	}
}

func TestAddRefusesAZipEntryThatEscapesTheBundle(t *testing.T) {
	for _, evil := range []string{"../evil", "toy-S/../../evil", "/tmp/evil", `..\evil`, "C:/evil", `C:\evil`} {
		t.Run(evil, func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "data")
			archive := writeFile(t, filepath.Join(t.TempDir(), "toy-S.zip"), zipOf(t, writeBundle(t, t.TempDir(), "toy-S", "S"), "", evil))
			_, err := Add(context.Background(), data, archive, pass, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "escapes") {
				t.Fatalf("error %v; want a refusal of the escaping entry", err)
			}
			if got := entries(t, filepath.Join(data, "models")); len(got) != 0 {
				t.Errorf("models/ holds %q after a refusal", got)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(data), "evil")); !os.IsNotExist(err) {
				t.Errorf("the escaping entry was written: %v", err)
			}
		})
	}
}

func TestAddLeavesNoBundleWhenTheCompatibilityTestFails(t *testing.T) {
	data := t.TempDir()
	archive := writeFile(t, filepath.Join(t.TempDir(), "toy-S.zip"), zipOf(t, writeBundle(t, t.TempDir(), "toy-S", "S"), "toy-S/"))
	refuse := func(string) error { return errors.New("stave_infer: manifest seed: 7 is not 12") }
	_, err := Add(context.Background(), data, archive, refuse, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "seed: 7 is not 12") || !strings.HasPrefix(err.Error(), archive+": ") {
		t.Fatalf("error %v; want the source named with the compatibility test's reason", err)
	}
	if got := entries(t, filepath.Join(data, "models")); len(got) != 0 {
		t.Errorf("models/ holds %q after a refused bundle", got)
	}
	if c, _ := app.LoadConfig(data); c.Model != "" {
		t.Errorf("a refused bundle became active: %q", c.Model)
	}
}

func TestAddRefusesABundleThatFailsItsHashes(t *testing.T) {
	data := t.TempDir()
	src := writeBundle(t, t.TempDir(), "toy-S", "S")
	writeFile(t, filepath.Join(src, "tokenizer", "merges.txt"), []byte("changed"))
	_, err := Add(context.Background(), data, src, pass, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "merges.txt") {
		t.Fatalf("error %v; want the tampered file named", err)
	}
	if got := entries(t, filepath.Join(data, "models")); len(got) != 0 {
		t.Errorf("models/ holds %q after a refusal", got)
	}
}

// Unpinned tokenizer sidecars can change inference; file-browser metadata cannot.
func TestAddRefusesAnUnpinnedFileInTheTokenizerDirectory(t *testing.T) {
	data := t.TempDir()
	src := writeBundle(t, t.TempDir(), "toy-S", "S")
	writeFile(t, filepath.Join(src, "tokenizer", "added_tokens.json"), []byte(`{"foobar": 10}`))
	const want = "tokenizer.dir: holds unpinned entries: added_tokens.json"
	if err := VerifyBundle(src); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("VerifyBundle: %v; want %q", err, want)
	}
	if _, err := Add(context.Background(), data, src, pass, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Add: %v; want %q", err, want)
	}
	if got := entries(t, filepath.Join(data, "models")); len(got) != 0 {
		t.Errorf("models/ holds %q after a refusal", got)
	}
	if err := os.Remove(filepath.Join(src, "tokenizer", "added_tokens.json")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".DS_Store", "Thumbs.db"} {
		writeFile(t, filepath.Join(src, "tokenizer", name), []byte(name))
	}
	if _, err := Add(context.Background(), data, src, pass, &bytes.Buffer{}); err != nil {
		t.Errorf("Add with .DS_Store and Thumbs.db beside the pinned files: %v", err)
	}
}

func TestAddInstallsADirectoryAndActivatesOnlyTheFirstBundle(t *testing.T) {
	data := t.TempDir()
	var out bytes.Buffer
	for _, b := range []struct{ id, variant string }{{"toy-B", "B"}, {"toy-S", "S"}} {
		if _, err := Add(context.Background(), data, writeBundle(t, t.TempDir(), b.id, b.variant), pass, &out); err != nil {
			t.Fatal(err)
		}
	}
	m, dir, err := Active(data)
	if err != nil || m.ID != "toy-B" || dir != filepath.Join(data, "models", "toy-B") {
		t.Errorf("Active = %s %s %v; want the first bundle added", m.ID, dir, err)
	}
	if !strings.Contains(out.String(), "toy-S") || !strings.Contains(out.String(), "active model: toy-B") {
		t.Errorf("output:\n%s", out.String())
	}
	installed, err := Installed(data)
	if err != nil || len(installed) != 2 || installed[0].ID != "toy-B" || installed[1].ID != "toy-S" {
		t.Errorf("Installed = %v, %v", installed, err)
	}
	if _, err := Add(context.Background(), data, reweigh(t, writeBundle(t, t.TempDir(), "toy-S", "S")), pass, &out); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Errorf("re-adding an installed id: %v", err)
	}
}

// reweigh gives the bundle in dir other weights, pinned by its manifest, and returns dir.
func reweigh(t *testing.T, dir string) string {
	t.Helper()
	weights := []byte("other weights")
	writeFile(t, filepath.Join(dir, "model.pt"), weights)
	return editManifest(t, dir, func(m map[string]any) {
		m["model"].(map[string]any)["sha256"], m["model"].(map[string]any)["bytes"] = hexOf(weights), len(weights)
	})
}

// editManifest applies edit to the manifest of the bundle in dir and returns dir.
func editManifest(t *testing.T, dir string, edit func(m map[string]any)) string {
	t.Helper()
	var m map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	data, _ = json.Marshal(m)
	writeFile(t, filepath.Join(dir, "manifest.json"), data)
	return dir
}

// Repeated setup reuses matching manifests but refuses a different manifest under an existing ID.
func TestAddReusesAnIdenticalInstalledBundle(t *testing.T) {
	data := t.TempDir()
	src := writeBundle(t, t.TempDir(), "toy-S", "S")
	for i := range 2 {
		var out bytes.Buffer
		m, err := Add(context.Background(), data, src, pass, &out)
		if err != nil || m.ID != "toy-S" {
			t.Fatalf("Add = %s, %v\n%s", m.ID, err, out.String())
		}
		if i == 1 && !strings.Contains(out.String(), "model toy-S is already installed; reused") {
			t.Errorf("second add output:\n%s", out.String())
		}
	}
	if got := entries(t, filepath.Join(data, "models")); !slices.Equal(got, []string{"toy-S"}) {
		t.Errorf("models/ holds %q, want one installation", got)
	}
	if _, err := Add(context.Background(), data, reweigh(t, writeBundle(t, t.TempDir(), "toy-S", "S")), pass, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already installed; remove it first") {
		t.Errorf("a different bundle under an installed id: %v; want the refusal", err)
	}
	other := editManifest(t, writeBundle(t, t.TempDir(), "toy-S", "S"), func(m map[string]any) { m["description"] = "other" })
	if _, err := Add(context.Background(), data, other, pass, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already installed; remove it first") {
		t.Errorf("a bundle that differs in its description only: %v; want the refusal", err)
	}
	if got := entries(t, filepath.Join(data, "models")); !slices.Equal(got, []string{"toy-S"}) {
		t.Errorf("models/ holds %q after the refusals", got)
	}
	if err := os.Remove(filepath.Join(data, app.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := Add(context.Background(), data, src, pass, &out); err != nil {
		t.Fatal(err)
	}
	if m, _, err := Active(data); err != nil || m.ID != "toy-S" || !strings.Contains(out.String(), "active model: toy-S") {
		t.Errorf("after a reuse without config.json: active %s, %v; output:\n%s", m.ID, err, out.String())
	}
}

func TestAddDownloadsACatalogBundleAndRefusesAWrongHashBeforeUnzipping(t *testing.T) {
	archive := zipOf(t, writeBundle(t, t.TempDir(), "toy-S", "S"), "toy-S/")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/garbage.zip" {
			w.Write([]byte("not a zip"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		w.Write(archive)
	}))
	defer srv.Close()

	// The bytes are not even a zip. So only a refusal before unzipping names the hash and not the archive.
	data := t.TempDir()
	wrong := Catalog{Default: "toy-S", Bundles: map[string]Entry{"toy-S": {Variant: "S", URL: srv.URL + "/garbage.zip", SHA256: hexOf(archive)}}}
	_, err := add(context.Background(), data, "toy-S", wrong, pass, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not the catalog's "+hexOf(archive)) {
		t.Fatalf("error %v; want the hash mismatch", err)
	}
	if len(entries(t, filepath.Join(data, "models"))) != 0 || len(entries(t, filepath.Join(data, "downloads"))) != 0 {
		t.Errorf("a download with the wrong hash left models %q, downloads %q", entries(t, filepath.Join(data, "models")), entries(t, filepath.Join(data, "downloads")))
	}

	right := Catalog{Default: "toy-S", Bundles: map[string]Entry{"toy-S": {Variant: "S", URL: srv.URL + "/toy-S.zip", SHA256: hexOf(archive)}}}
	var out bytes.Buffer
	if _, err := add(context.Background(), data, "toy-S", right, pass, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "models", "toy-S", "manifest.json")); err != nil {
		t.Errorf("the catalog bundle was not installed: %v", err)
	}
	if !strings.Contains(out.String(), "100% of ") || strings.Contains(out.String(), "\r") {
		t.Errorf("output does not show the download's progress in plain lines:\n%s", out.String())
	}
}

func TestADownloadOfUnknownLengthShowsItsProgressInMegabytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for range 51 {
			w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	var out bytes.Buffer
	if _, err := add(context.Background(), t.TempDir(), srv.URL+"/b.zip", Catalog{}, pass, &out); err == nil {
		t.Fatal("a body of zeros was added as a bundle")
	}
	if !strings.Contains(out.String(), "  50 MB\n") || strings.Contains(out.String(), "%") {
		t.Errorf("output does not show the downloaded megabytes:\n%s", out.String())
	}
}

// Reusing an installed catalog ID must not require a download.
func TestAddReusesAnInstalledCatalogBundleWithoutDownloadingIt(t *testing.T) {
	archive := zipOf(t, writeBundle(t, t.TempDir(), "toy-S", "S"), "toy-S/")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(archive)
	}))
	defer srv.Close()
	data := t.TempDir()
	if _, err := Add(context.Background(), data, writeBundle(t, t.TempDir(), "toy-S", "S"), pass, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(data, app.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	cat := Catalog{Default: "toy-S", Bundles: map[string]Entry{"toy-S": {Variant: "S", URL: srv.URL + "/toy-S.zip", SHA256: hexOf(archive)}}}
	var out bytes.Buffer
	m, err := add(context.Background(), data, "toy-S", cat, pass, &out)
	if err != nil || m.ID != "toy-S" {
		t.Fatalf("add of an installed catalog id = %s, %v\n%s", m.ID, err, out.String())
	}
	if !strings.Contains(out.String(), "model toy-S is already installed; reused") || !strings.Contains(out.String(), "active model: toy-S") {
		t.Errorf("output:\n%s", out.String())
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the server received %d requests; want none for an installed bundle", n)
	}
	if m, _, err := Active(data); err != nil || m.ID != "toy-S" {
		t.Errorf("active after the reuse = %s, %v", m.ID, err)
	}
}

func TestAddOfAnUnpublishedCatalogBundleSaysToAddALocalOne(t *testing.T) {
	unpublished := Catalog{Default: "toy-S", Bundles: map[string]Entry{"toy-S": {Variant: "S"}}}
	_, err := add(context.Background(), t.TempDir(), "toy-S", unpublished, pass, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "toy-S is not published yet") || !strings.Contains(err.Error(), "local zip or directory") {
		t.Errorf("error %v", err)
	}
}

// Model assets can be published after the binary, so distinguish a missing asset from other download failures.
func TestAddOfACatalogBundleNotUploadedYetSaysItIsNotPublished(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	data := t.TempDir()
	pending := Catalog{Default: "toy-S", Bundles: map[string]Entry{"toy-S": {Variant: "S", URL: srv.URL + "/toy-S.zip", SHA256: strings.Repeat("a", 64)}}}
	_, err := add(context.Background(), data, "toy-S", pending, pass, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "toy-S is not published yet") || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "local zip or directory") {
		t.Errorf("error %v; want not published yet with the 404", err)
	}
	if len(entries(t, filepath.Join(data, "models"))) != 0 || len(entries(t, filepath.Join(data, "downloads"))) != 0 {
		t.Errorf("a missing download left models %q, downloads %q", entries(t, filepath.Join(data, "models")), entries(t, filepath.Join(data, "downloads")))
	}
}

func TestAddOfAnUnknownSourceNamesTheCatalog(t *testing.T) {
	_, err := Add(context.Background(), t.TempDir(), "Q-1", pass, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "S-20261020") {
		t.Errorf("error %v; want the catalog ids named", err)
	}
}

func TestUseSwitchesAndAnUnknownIDNamesTheInstalledOnes(t *testing.T) {
	data := t.TempDir()
	for _, b := range []struct{ id, variant string }{{"toy-S", "S"}, {"toy-B", "B"}} {
		if _, err := Add(context.Background(), data, writeBundle(t, t.TempDir(), b.id, b.variant), pass, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Use(data, "toy-B"); err != nil {
		t.Fatal(err)
	}
	if m, _, err := Active(data); err != nil || m.ID != "toy-B" {
		t.Errorf("Active after use = %s, %v", m.ID, err)
	}
	err := Use(data, "toy-F")
	if err == nil || !strings.Contains(err.Error(), "toy-B, toy-S") {
		t.Errorf("Use of an unknown id: %v; want the installed ids named", err)
	}
}

func TestRemoveRefusesTheActiveBundle(t *testing.T) {
	data := t.TempDir()
	for _, b := range []struct{ id, variant string }{{"toy-S", "S"}, {"toy-B", "B"}} {
		if _, err := Add(context.Background(), data, writeBundle(t, t.TempDir(), b.id, b.variant), pass, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(data, "toy-S"); err == nil || !strings.Contains(err.Error(), "active") {
		t.Errorf("removing the active bundle: %v", err)
	}
	if err := Remove(data, "toy-B"); err != nil {
		t.Errorf("removing an inactive bundle: %v", err)
	}
	if got := entries(t, filepath.Join(data, "models")); !slices.Equal(got, []string{"toy-S"}) {
		t.Errorf("models/ = %q", got)
	}
	for _, id := range []string{"toy-B", "../models", ""} {
		if err := Remove(data, id); err == nil {
			t.Errorf("Remove(%q) succeeded", id)
		}
	}
}

func TestActiveStatesWhyNoBundleIsAvailable(t *testing.T) {
	data := t.TempDir()
	if _, _, err := Active(data); !errors.Is(err, ErrNoModel) || err.Error() != "no model installed; run stave setup" {
		t.Errorf("no config: %v", err)
	}
	if err := app.SaveConfig(data, app.Config{Model: "S-20261010", AdvisoryTimeout: 1}); err != nil {
		t.Fatal(err)
	}
	var missing *NotInstalledError
	if _, _, err := Active(data); !errors.As(err, &missing) || err.Error() != "model S-20261010 is not installed; run stave model list" {
		t.Errorf("configured but absent: %v", err)
	}
}

func TestReadManifestRefusesWhatThisRuntimeCannotServe(t *testing.T) {
	for field, edit := range map[string]func(map[string]any){
		"format":  func(m map[string]any) { m["format"] = "stave-model-bundle-2" },
		"id":      func(m map[string]any) { m["id"] = "../x" },
		"variant": func(m map[string]any) { m["variant"] = "B" },
		"model.format": func(m map[string]any) {
			m["model"].(map[string]any)["format"] = "stave-structural-7"
		},
		"tasks":      func(m map[string]any) { m["tasks"] = []string{"P1", "P2", "P3", "TF", "DC"} },
		"model.file": func(m map[string]any) { m["model"].(map[string]any)["file"] = "../model.pt" },
		"tokenizer.files": func(m map[string]any) {
			delete(m["tokenizer"].(map[string]any)["files"].(map[string]any), "merges.txt")
		},
		"backbone.config": func(m map[string]any) { m["backbone"].(map[string]any)["config"] = "/etc/passwd" },
		"tokenizer.dir":   func(m map[string]any) { m["tokenizer"].(map[string]any)["dir"] = `..\tok` },
	} {
		t.Run(field, func(t *testing.T) {
			dir := writeBundle(t, t.TempDir(), "toy-S", "S")
			var m map[string]any
			data, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			edit(m)
			data, _ = json.Marshal(m)
			writeFile(t, filepath.Join(dir, "manifest.json"), data)
			if _, err := ReadManifest(dir); err == nil || !strings.Contains(err.Error(), "manifest "+field) {
				t.Errorf("error %v; want one naming manifest %s", err, field)
			}
		})
	}
}

func TestTheEmbeddedCatalogNamesTheThreeBundles(t *testing.T) {
	c := EmbeddedCatalog()
	if c.Default != "S-20261020" {
		t.Errorf("default %q", c.Default)
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for id, variant := range map[string]string{"S-20261020": "S", "B-20261020": "B", "F-20261020": "F"} {
		e, ok := c.Bundles[id]
		if !ok || e.Variant != variant {
			t.Errorf("%s: %+v, %v", id, e, ok)
		}
		if e.URL == "" {
			if e.SHA256 != "" {
				t.Errorf("%s: unpublished bundle has sha256 %q", id, e.SHA256)
			}
			continue
		}
		// Published model assets have HTTPS download URLs and pinned checksums.
		if u, err := url.ParseRequestURI(e.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("%s: url %q is not an absolute HTTPS URL", id, e.URL)
		}
		if !hex64.MatchString(e.SHA256) {
			t.Errorf("%s: sha256 %q is not 64 lowercase hex characters", id, e.SHA256)
		}
	}
	if len(c.Bundles) != 3 {
		t.Errorf("%d bundles", len(c.Bundles))
	}
}

// Verify that Python-written manifests remain readable by Go.
func TestAToyBundleFromTheRuntimeReadsAndVerifies(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a toy bundle with the Python package")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not on PATH")
	}
	infer, _ := filepath.Abs(filepath.Join("..", "..", "infer"))
	out := filepath.Join(t.TempDir(), "toy-B")
	cmd := exec.Command("uv", "run", "--directory", infer, "--locked", "python", "-m", "stave_infer.toy",
		"--variant", "B", "--out", out, "--tokenizer-dir", filepath.Join(infer, "tests", "fixtures", "tokenizer"))
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("toy bundle: %v\n%s", err, msg)
	}
	m, err := ReadManifest(out)
	if err != nil || m.ID != "toy-B" || m.Variant != "B" || m.Model.Format != "stave-flat-1" {
		t.Fatalf("ReadManifest = %+v, %v", m, err)
	}
	if err := VerifyBundle(out); err != nil {
		t.Error(err)
	}
}
