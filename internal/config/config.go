// Package config reads agentsync's TOML config file, which may name the
// canonical repo and the default tree of projects.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// Config holds the settings the file may carry. An empty field means the key
// was absent: Load refuses a key that is present but empty.
type Config struct {
	Repo string `toml:"repo"`
	Root string `toml:"root"`
}

// Path is where the config file lives: under $XDG_CONFIG_HOME when that is
// absolute, else under ~/.config, on macOS too.
func Path() (string, error) {
	// The XDG spec says a relative value is invalid and must be ignored.
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "agentsync", "config.toml"), nil
	}
	home, err := home()
	if err != nil {
		return "", fmt.Errorf("cannot locate the config file: %w", err)
	}
	return filepath.Join(home, ".config", "agentsync", "config.toml"), nil
}

// Load reads the file at path. A missing file is an empty Config, not an
// error; every other problem with it is an error naming the file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A link whose target is gone is a broken config, not an absent one.
		if _, lerr := os.Lstat(path); lerr == nil {
			return Config{}, fmt.Errorf("config file %s is a dangling symlink; fix or remove the link", path)
		}
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config file %s: %w", path, err)
	}

	var c Config
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return Config{}, fmt.Errorf("config file %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("config file %s: unknown key %q (known keys: repo, root)", path, undecoded[0].String())
	}

	for _, f := range []struct {
		key string
		val *string
	}{{"repo", &c.Repo}, {"root", &c.Root}} {
		if !md.IsDefined(f.key) {
			continue
		}
		v, err := ParsePath(f.key, *f.val)
		if err != nil {
			return Config{}, fmt.Errorf("config file %s: %w", path, err)
		}
		*f.val = v
	}
	return c, nil
}

// ParsePath checks one setting's value by the file's rules and returns it
// expanded: non-empty, and absolute once a leading ~ or ~/ is expanded against
// $HOME. key names the setting in the error.
func ParsePath(key, raw string) (string, error) {
	v, err := expand(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	// A written or file value has no working directory a relative path could mean.
	if !filepath.IsAbs(v) {
		return "", fmt.Errorf("%s must be an absolute path, ~, or start with ~/, got %q", key, raw)
	}
	// TOML holds only UTF-8, so Load would refuse a file carrying anything else.
	if !utf8.ValidString(v) {
		return "", fmt.Errorf("%s is not valid UTF-8, got %q", key, v)
	}
	return v, nil
}

// Render is the exact file Write would put on disk for c. Both values are
// required, and one under $HOME is written as ~/… so the file stays portable.
func Render(c Config) ([]byte, error) {
	var err error
	if c.Repo, err = portable("repo", c.Repo); err != nil {
		return nil, err
	}
	if c.Root, err = portable("root", c.Root); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = toml.NewEncoder(&buf).Encode(c)
	return buf.Bytes(), err
}

func portable(key, raw string) (string, error) {
	v, err := ParsePath(key, raw)
	if err != nil {
		return "", err
	}
	h, err := home()
	if err != nil {
		return v, nil
	}
	rel, err := filepath.Rel(h, v)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return v, nil
	}
	if rel == "." {
		return "~", nil
	}
	return "~/" + filepath.ToSlash(rel), nil
}

// Write puts Render(c) at path with mode 0600, creating its directory 0700. The
// file is staged as a temp file beside it and then hard-linked into place, which
// fails if path already exists; replace renames over it instead.
func Write(path string, c Config, replace bool) error {
	data, err := Render(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	// Each step's error falls through to one exit, so every failure is reported
	// the same way and none needs a branch of its own.
	err = os.MkdirAll(dir, 0o700)
	var f *os.File
	if err == nil {
		f, err = os.CreateTemp(dir, ".config-*.toml")
	}
	if err != nil {
		return fmt.Errorf("writing config file %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err == nil {
		place := os.Link
		if replace {
			place = os.Rename
		}
		err = place(tmp, path)
	}
	if err != nil {
		return fmt.Errorf("writing config file %s: %w", path, err)
	}
	return nil
}

func expand(v string) (string, error) {
	if v == "" {
		return "", errors.New("is empty; remove the key or give it a path")
	}
	if v == "~" {
		v = "~/"
	}
	rest, ok := strings.CutPrefix(v, "~/")
	if !ok {
		return v, nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("cannot expand ~: %w", err)
	}
	return filepath.Join(h, rest), nil
}

func home() (string, error) {
	h := os.Getenv("HOME")
	if !filepath.IsAbs(h) {
		return "", fmt.Errorf("$HOME must be an absolute path, got %q", h)
	}
	return h, nil
}
