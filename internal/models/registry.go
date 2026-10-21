package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/aholovko/stave/internal/app"
)

// ErrNoModel is the state of a data directory with no active bundle.
var ErrNoModel = errors.New("no model installed; run stave setup")

// errNotFound distinguishes a missing download from other HTTP failures.
var errNotFound = errors.New("404 Not Found")

// NotInstalledError is a bundle id that names no installed bundle.
type NotInstalledError struct{ ID string }

func (e *NotInstalledError) Error() string {
	return fmt.Sprintf("model %s is not installed; run stave model list", e.ID)
}

// Dir returns the installation directory of one bundle id.
func Dir(dataDir, id string) string {
	return filepath.Join(dataDir, "models", id)
}

// Bundle returns the manifest of the installed bundle id and its directory.
func Bundle(dataDir, id string) (Manifest, string, error) {
	if !idPattern.MatchString(id) {
		return Manifest{}, "", &NotInstalledError{ID: id}
	}
	dir := Dir(dataDir, id)
	m, err := ReadManifest(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Manifest{}, "", &NotInstalledError{ID: id}
	case err != nil:
		return Manifest{}, "", fmt.Errorf("model %s: %v", id, err)
	case m.ID != id:
		return Manifest{}, "", fmt.Errorf("model %s: %s holds bundle %s", id, dir, m.ID)
	}
	return m, dir, nil
}

// Active returns the bundle that config.json names, and its directory.
func Active(dataDir string) (Manifest, string, error) {
	c, err := app.LoadConfig(dataDir)
	if err != nil {
		return Manifest{}, "", err
	}
	if c.Model == "" {
		return Manifest{}, "", ErrNoModel
	}
	return Bundle(dataDir, c.Model)
}

// Installed lists the installed bundles in id order.
func Installed(dataDir string) ([]Manifest, error) {
	list, err := os.ReadDir(filepath.Join(dataDir, "models"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, e := range list {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") { // .add-* is an installation in progress
			continue
		}
		m, _, err := Bundle(dataDir, e.Name())
		if err != nil {
			return nil, fmt.Errorf("%v; remove it with stave model remove %s", err, e.Name())
		}
		out = append(out, m)
	}
	return out, nil
}

// Use makes an installed bundle the active one.
func Use(dataDir, id string) error {
	if _, _, err := Bundle(dataDir, id); err != nil {
		var missing *NotInstalledError
		if !errors.As(err, &missing) {
			return err
		}
		return fmt.Errorf("model %s is not installed; installed: %s", id, installedIDs(dataDir))
	}
	c, err := app.LoadConfig(dataDir)
	if err != nil {
		return err
	}
	c.Model = id
	return app.SaveConfig(dataDir, c)
}

// Remove deletes an installed bundle other than the active one.
func Remove(dataDir, id string) error {
	c, err := app.LoadConfig(dataDir)
	if err != nil {
		return err
	}
	if !idPattern.MatchString(id) {
		return &NotInstalledError{ID: id}
	}
	if _, err := os.Stat(Dir(dataDir, id)); err != nil {
		return &NotInstalledError{ID: id}
	}
	if id == c.Model {
		return fmt.Errorf("model %s is active; switch to another with stave model use ID before removing it", id)
	}
	return os.RemoveAll(Dir(dataDir, id))
}

func installedIDs(dataDir string) string {
	list, _ := Installed(dataDir)
	if len(list) == 0 {
		return "none (add one with stave model add)"
	}
	ids := make([]string, len(list))
	for i, m := range list {
		ids[i] = m.ID
	}
	return strings.Join(ids, ", ")
}

// Add installs from a catalog ID, URL, zip or directory, and selects it if no installed bundle is active.
// Installation is staged and verified with check before publication. Matching installed bundles are reused.
func Add(ctx context.Context, dataDir, source string, check func(dir string) error, w io.Writer) (Manifest, error) {
	return add(ctx, dataDir, source, EmbeddedCatalog(), check, w)
}

func add(ctx context.Context, dataDir, source string, cat Catalog, check func(dir string) error, w io.Writer) (Manifest, error) {
	if _, ok := cat.Bundles[source]; ok {
		if _, err := os.Stat(Dir(dataDir, source)); err == nil {
			// Reuse installed catalog IDs without downloading them again; installation already verified their contents.
			installed, err := ReadManifest(Dir(dataDir, source))
			if err != nil {
				return Manifest{}, errInstalled(source)
			}
			fmt.Fprintf(w, "model %s is already installed; reused\n", source)
			return activate(dataDir, installed, w)
		}
	}
	models := filepath.Join(dataDir, "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		return Manifest{}, err
	}
	stage, err := os.MkdirTemp(models, ".add-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(stage)

	if err := fetch(ctx, dataDir, source, cat, stage, w); err != nil {
		return Manifest{}, err
	}
	root, err := bundleRoot(stage)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %v", source, err)
	}
	m, err := ReadManifest(root)
	if err != nil {
		return Manifest{}, err
	}
	target := Dir(dataDir, m.ID)
	if _, err := os.Stat(target); err == nil {
		// Rerunning setup with the same bundle should reuse the installation.
		installed, err := ReadManifest(target)
		if err != nil || !reflect.DeepEqual(installed, m) {
			return Manifest{}, errInstalled(m.ID)
		}
		fmt.Fprintf(w, "model %s is already installed; reused\n", m.ID)
		return activate(dataDir, installed, w)
	}
	if err := VerifyBundle(root); err != nil {
		return Manifest{}, err
	}
	if err := check(root); err != nil {
		return Manifest{}, fmt.Errorf("%s: %v", source, err)
	}
	if err := os.Rename(root, target); err != nil {
		return Manifest{}, err
	}
	fmt.Fprintf(w, "added %s (variant %s, version %s, model sha256 %s) in %s\n", m.ID, m.Variant, m.Version, DigestPrefix(m.Model.SHA256), target)
	return activate(dataDir, m, w)
}

// activate makes m the active bundle when no installed bundle is active, and states the active one.
func activate(dataDir string, m Manifest, w io.Writer) (Manifest, error) {
	c, err := app.LoadConfig(dataDir)
	if err != nil {
		return m, err
	}
	if _, _, err := Bundle(dataDir, c.Model); c.Model == "" || err != nil {
		c.Model = m.ID
		if err := app.SaveConfig(dataDir, c); err != nil {
			return m, err
		}
	}
	fmt.Fprintf(w, "active model: %s\n", c.Model)
	return m, nil
}

func errInstalled(id string) error {
	return fmt.Errorf("model %s is already installed; remove it first with stave model remove %s", id, id)
}

// fetch puts the bundle files of the source into stage.
func fetch(ctx context.Context, dataDir, source string, cat Catalog, stage string, w io.Writer) error {
	if e, ok := cat.Bundles[source]; ok {
		if e.URL == "" {
			return fmt.Errorf("bundle %s is not published yet; a local zip or directory can be added instead: stave model add PATH", source)
		}
		err := download(ctx, dataDir, e.URL, e.SHA256, stage, w)
		if errors.Is(err, errNotFound) {
			return fmt.Errorf("bundle %s is not published yet (%v); a local zip or directory can be added instead: stave model add PATH", source, err)
		}
		return err
	}
	// Explicit URLs may use HTTP. Catalog URLs retain their checksum pin even when supplied directly.
	if strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://") {
		pinned := ""
		for _, e := range cat.Bundles {
			if e.URL == source {
				pinned = e.SHA256
			}
		}
		return download(ctx, dataDir, source, pinned, stage, w)
	}
	info, err := os.Stat(source)
	switch {
	case err != nil:
		return fmt.Errorf("%s is not a catalog id (%s), a URL, a zip file or a bundle directory", source, strings.Join(cat.IDs(), ", "))
	case info.IsDir():
		return copyTree(source, stage)
	}
	return unzip(source, stage)
}

const mb = 1_000_000

// progress prints a line on w at every tenth of the total bytes written, or every 50 MB when the total is unknown.
type progress struct {
	w           io.Writer
	total, done int64
	step        int64 // the last step printed
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.total > 0 {
		if step := p.done * 10 / p.total; step > p.step {
			p.step = step
			fmt.Fprintf(p.w, "  %d%% of %d MB\n", step*10, p.total/mb)
		}
	} else if step := p.done / (50 * mb); step > p.step {
		p.step = step
		fmt.Fprintf(p.w, "  %d MB\n", step*50)
	}
	return len(b), nil
}

// download verifies any catalog checksum before extraction and removes the downloaded zip on success or failure.
func download(ctx context.Context, dataDir, url, pinned, stage string, w io.Writer) error {
	downloads := filepath.Join(dataDir, "downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(downloads, "bundle-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	fmt.Fprintf(w, "downloading %s\n", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("download %s: %w", url, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h, &progress{w: w, total: resp.ContentLength}), resp.Body); err != nil {
		return fmt.Errorf("download %s: %v", url, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); pinned != "" && got != pinned {
		return fmt.Errorf("download %s has sha256 %s, not the catalog's %s; refused", path.Base(url), got, pinned)
	}
	return unzip(f.Name(), stage)
}

// bundleRoot returns the directory that holds manifest.json. It is the extraction directory itself, or its single
// top-level directory.
func bundleRoot(stage string) (string, error) {
	if _, err := os.Stat(filepath.Join(stage, ManifestFile)); err == nil {
		return stage, nil
	}
	list, err := os.ReadDir(stage)
	if err != nil {
		return "", err
	}
	if len(list) == 1 && list[0].IsDir() {
		root := filepath.Join(stage, list[0].Name())
		if _, err := os.Stat(filepath.Join(root, ManifestFile)); err == nil {
			return root, nil
		}
	}
	return "", errors.New("no manifest.json at the top of the bundle or of its single top-level directory")
}

// DigestPrefix returns the displayed part of a model digest.
func DigestPrefix(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
