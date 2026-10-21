package models

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Validate every zip entry before writing so unsafe paths or special files cannot leave a partial extraction.
func unzip(archive, dir string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("%s: %v", archive, err)
	}
	defer r.Close()
	names := make([]string, len(r.File))
	for i, f := range r.File {
		name, err := entryName(f.Name)
		if err != nil {
			return fmt.Errorf("%s: %v", archive, err)
		}
		if mode := f.Mode(); !mode.IsRegular() && !mode.IsDir() {
			return fmt.Errorf("%s: entry %q is not a file or a directory", archive, f.Name)
		}
		names[i] = name
	}
	for i, f := range r.File {
		target := filepath.Join(dir, filepath.FromSlash(names[i]))
		if f.Mode().IsDir() || names[i] == "." {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := extract(f, target); err != nil {
			return fmt.Errorf("%s: %s: %v", archive, f.Name, err)
		}
	}
	return nil
}

// entryName rejects paths that would escape the extraction directory.
func entryName(name string) (string, error) {
	n := strings.ReplaceAll(name, `\`, "/")
	if path.IsAbs(n) || filepath.IsAbs(name) || (len(n) >= 2 && n[1] == ':') {
		return "", fmt.Errorf("entry %q escapes the bundle: it is an absolute path", name)
	}
	n = path.Clean(n)
	if n == ".." || strings.HasPrefix(n, "../") {
		return "", fmt.Errorf("entry %q escapes the bundle through ..", name)
	}
	return n, nil
}

func extract(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTree copies the bundle directory src into dir. Only files and directories are copied.
func copyTree(src, dir string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case !d.Type().IsRegular():
			return fmt.Errorf("%s is not a file or a directory", p)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
