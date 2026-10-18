package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPath(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	tests := []struct {
		name, xdg, home, want string
	}{
		{"XDG_CONFIG_HOME set", xdg, home, filepath.Join(xdg, "agentsync", "config.toml")},
		{"XDG_CONFIG_HOME unset", "", home, filepath.Join(home, ".config", "agentsync", "config.toml")},
		{"relative XDG_CONFIG_HOME is ignored", "rel/xdg", home, filepath.Join(home, ".config", "agentsync", "config.toml")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			t.Setenv("HOME", tc.home)
			got, err := Path()
			if err != nil || got != tc.want {
				t.Errorf("Path() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestPath_NoUsableHomeRefuses(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, h := range []string{"", "relative"} {
		t.Setenv("HOME", h)
		if got, err := Path(); err == nil {
			t.Errorf("HOME=%q: Path() = %q, want an error", h, got)
		}
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct {
		name, content string
		want          Config
	}{
		{"tilde expands against HOME", "repo = \"~/agents\"\nroot = \"~/Web\"\n",
			Config{Repo: filepath.Join(home, "agents"), Root: filepath.Join(home, "Web")}},
		{"bare tilde and tilde slash both mean HOME", "repo = \"~\"\nroot = \"~/\"\n",
			Config{Repo: home, Root: home}},
		{"absolute paths pass through", "repo = \"/opt/agents\"\nroot = \"/srv\"\n",
			Config{Repo: "/opt/agents", Root: "/srv"}},
		{"one key leaves the other unset", "repo = \"/opt/agents\"\n", Config{Repo: "/opt/agents"}},
		{"an empty file sets nothing", "", Config{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Load(writeConfig(t, tc.content))
			if err != nil || got != tc.want {
				t.Errorf("Load = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil || got != (Config{}) {
		t.Errorf("Load = %+v, %v; want an empty Config and no error", got, err)
	}
}

func TestLoad_RefusesBrokenFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name, content string
		want          []string
	}{
		{"parse error names the line", "repo = \"/a\"\nroot = \n", []string{"line 2"}},
		{"unknown key is named", "rpeo = \"/a\"\n", []string{`"rpeo"`, "unknown key"}},
		{"unknown table is named", "[extra]\nx = 1\n", []string{`"extra"`}},
		{"wrong type", "repo = 5\n", []string{`"repo"`, "incompatible types"}},
		{"empty value", "repo = \"\"\n", []string{"repo: is empty"}},
		{"blank value", "root = \"   \"\n", []string{"root: is empty"}},
		{"relative root", "root = \"Web\"\n", []string{"root must be an absolute path"}},
		{"relative repo", "repo = \"./agents\"\n", []string{"repo must be an absolute path"}},
		{"tilde user is not expanded", "root = \"~bob/Web\"\n", []string{"root must be an absolute path"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.content)
			got, err := Load(path)
			if err == nil {
				t.Fatalf("Load = %+v, want an error", got)
			}
			assertNames(t, err, path, tc.want...)
		})
	}
}

// assertNames checks err names path and each of want, matching want against
// the message with path removed: t.TempDir puts the subtest name in path.
func assertNames(t *testing.T, err error, path string, want ...string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want one naming %s", err, path)
	}
	msg := strings.ReplaceAll(err.Error(), path, "<path>")
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("error does not say %q:\n%s", w, msg)
		}
	}
}

func TestLoad_DirectoryRefuses(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	assertNames(t, err, dir, "is a directory")
}

func TestLoad_DanglingSymlinkRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.Symlink(filepath.Join(dir, "gone.toml"), path); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	assertNames(t, err, path, "dangling symlink")
}

func TestLoad_TildeWithoutHomeRefuses(t *testing.T) {
	path := writeConfig(t, "repo = \"~/agents\"\n")
	t.Setenv("HOME", "")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "$HOME") {
		t.Errorf("err = %v, want a refusal naming $HOME", err)
	}
}
