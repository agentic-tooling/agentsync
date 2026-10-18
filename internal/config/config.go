// Package config reads agentsync's TOML config file, which may name the
// canonical repo and the default tree of projects.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
		v, err := expand(strings.TrimSpace(*f.val))
		if err != nil {
			return Config{}, fmt.Errorf("config file %s: %s: %w", path, f.key, err)
		}
		// Unlike a flag or env var, a file has no working directory a relative
		// path could sensibly mean.
		if !filepath.IsAbs(v) {
			return Config{}, fmt.Errorf("config file %s: %s must be an absolute path or start with ~/, got %q", path, f.key, *f.val)
		}
		*f.val = v
	}
	return c, nil
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
