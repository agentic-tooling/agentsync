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

func TestParsePath_RefusalNamesBareTilde(t *testing.T) {
	_, err := ParsePath("root", "Web")
	if err == nil || !strings.Contains(err.Error(), `root must be an absolute path, ~, or start with ~/, got "Web"`) {
		t.Errorf("err = %v", err)
	}
}

func TestParsePath_RefusesInvalidUTF8(t *testing.T) {
	_, err := ParsePath("repo", "/opt/a\xffb")
	if err == nil || !strings.Contains(err.Error(), "repo is not valid UTF-8") {
		t.Errorf("err = %v, want a UTF-8 refusal", err)
	}
	if _, err := Render(Config{Repo: "/opt/a\xffb", Root: "/opt"}); err == nil {
		t.Error("Render accepted a value Load would refuse")
	}
}

func TestRender(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct {
		name string
		in   Config
		want string
	}{
		{"under HOME is written with ~", Config{Repo: filepath.Join(home, "Web", "agents"), Root: filepath.Join(home, "Web")},
			"repo = \"~/Web/agents\"\nroot = \"~/Web\"\n"},
		{"tilde input round-trips", Config{Repo: "~/agents", Root: "~"}, "repo = \"~/agents\"\nroot = \"~\"\n"},
		{"outside HOME stays absolute", Config{Repo: "/opt/agents", Root: "/srv"}, "repo = \"/opt/agents\"\nroot = \"/srv\"\n"},
		{"a sibling of HOME is not under it", Config{Repo: home + "x/agents", Root: "/srv"},
			"repo = \"" + home + "x/agents\"\nroot = \"/srv\"\n"},
		{"quotes are escaped", Config{Repo: `/opt/a "b"`, Root: "/srv"}, "repo = \"/opt/a \\\"b\\\"\"\nroot = \"/srv\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(tc.in)
			if err != nil || string(got) != tc.want {
				t.Errorf("Render = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestRender_NoUsableHomeWritesAbsolute(t *testing.T) {
	t.Setenv("HOME", "")
	got, err := Render(Config{Repo: "/opt/agents", Root: "/srv"})
	if want := "repo = \"/opt/agents\"\nroot = \"/srv\"\n"; err != nil || string(got) != want {
		t.Errorf("Render = %q, %v; want %q", got, err, want)
	}
}

func TestRender_RefusesWhatLoadWould(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name string
		in   Config
		want string
	}{
		{"empty repo", Config{Root: "/srv"}, "repo: is empty"},
		{"empty root", Config{Repo: "/opt/agents"}, "root: is empty"},
		{"relative root", Config{Repo: "/opt/agents", Root: "Web"}, "root must be an absolute path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Render(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one saying %q", err, tc.want)
			}
		})
	}
}

// configDir returns a path for config.toml in a directory that does not exist
// yet, so Write has to create it.
func configDir(t *testing.T) (dir, path string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "agentsync")
	return dir, filepath.Join(dir, "config.toml")
}

// assertNoTemp fails if Write left its staging file behind in dir.
func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	tmps, err := filepath.Glob(filepath.Join(dir, ".config-*.toml"))
	if err != nil || len(tmps) != 0 {
		t.Errorf("temp files left behind: %v %v", tmps, err)
	}
}

func TestWrite_CreatesTheFileLoadReadsBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, path := configDir(t)
	want := Config{Repo: filepath.Join(home, "Web", "agents"), Root: filepath.Join(home, "Web")}

	if err := Write(path, want, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "repo = \"~/Web/agents\"\nroot = \"~/Web\"\n" {
		t.Errorf("file = %q, %v", data, err)
	}
	for p, mode := range map[string]os.FileMode{path: 0o600, dir: 0o700 | os.ModeDir} {
		if fi, err := os.Stat(p); err != nil || fi.Mode() != mode {
			t.Errorf("%s: mode = %v, %v; want %v", p, fi.Mode(), err, mode)
		}
	}
	if got, err := Load(path); err != nil || got != want {
		t.Errorf("Load = %+v, %v; want %+v", got, err, want)
	}
	assertNoTemp(t, dir)
}

func TestWrite_ExistingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Config{Repo: "/opt/agents", Root: "/srv"}

	t.Run("refused without replace and left alone", func(t *testing.T) {
		dir, path := configDir(t)
		mustWriteFile(t, path, "old\n")
		err := Write(path, c, false)
		assertNames(t, err, path, "file exists")
		if data, _ := os.ReadFile(path); string(data) != "old\n" {
			t.Errorf("file = %q, want it untouched", data)
		}
		assertNoTemp(t, dir)
	})

	t.Run("replaced with replace", func(t *testing.T) {
		dir, path := configDir(t)
		mustWriteFile(t, path, "old\n")
		if err := Write(path, c, true); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got, err := Load(path); err != nil || got != c {
			t.Errorf("Load = %+v, %v; want %+v", got, err, c)
		}
		assertNoTemp(t, dir)
	})

	t.Run("a directory in the way is not replaced", func(t *testing.T) {
		dir, path := configDir(t)
		mustWriteFile(t, filepath.Join(path, "keep"), "x")
		err := Write(path, c, true)
		if err == nil || !strings.HasPrefix(err.Error(), "writing config file "+path) {
			t.Errorf("err = %v, want a refusal naming %s", err, path)
		}
		assertNoTemp(t, dir)
	})
}

func TestWrite_RefusesBeforeTouchingDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, path := configDir(t)
	if err := Write(path, Config{Repo: "relative"}, false); err == nil || !strings.Contains(err.Error(), "repo must be an absolute path") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("Write created %s before validating: %v", dir, err)
	}
}

func TestWrite_UncreatableDirRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	parent := filepath.Join(t.TempDir(), "file")
	mustWriteFile(t, parent, "not a directory")
	path := filepath.Join(parent, "agentsync", "config.toml")
	err := Write(path, Config{Repo: "/opt/agents", Root: "/srv"}, false)
	assertNames(t, err, path, "not a directory")
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
