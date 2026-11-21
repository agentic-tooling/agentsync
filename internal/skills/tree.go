// Package skills inspects and provisions project-local Claude skill trees.
package skills

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

type node struct {
	path string
	mode fs.FileMode
	data []byte
}

type tree []node

// openDir holds each opened directory while descending so resolution stays
// beneath the original root even if an ancestor is renamed during the walk.
func openDir(root *os.Root, path string, create bool) (*os.Root, error) {
	parent := root
	for component := range strings.SplitSeq(path, string(filepath.Separator)) {
		next, err := openChild(parent, component, create)
		if parent != root {
			_ = parent.Close()
		}
		if err != nil {
			return nil, err
		}
		parent = next
	}
	return parent, nil
}

func openChild(parent *os.Root, name string, create bool) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if create && errors.Is(err, fs.ErrNotExist) {
		if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a real directory", filepath.Join(parent.Name(), name))
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	current, err := child.Stat(".")
	if err != nil || !os.SameFile(info, current) {
		_ = child.Close()
		return nil, fmt.Errorf("%s changed while opening it", child.Name())
	}
	return child, nil
}

var errNoManifest = errors.New("no regular SKILL.md")

func readTree(root *os.Root) (tree, error) {
	info, err := root.Lstat("SKILL.md")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoManifest
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", root.Name(), errNoManifest)
	}
	var result tree
	if err := readNodes(root, "", &result); err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(result, func(n node) bool { return n.path == "SKILL.md" && n.mode.IsRegular() }) {
		return nil, fmt.Errorf("%s changed while reading SKILL.md", root.Name())
	}
	slices.SortFunc(result, func(a, b node) int { return strings.Compare(a.path, b.path) })
	return result, nil
}

func openAbsolute(path string) (*os.Root, error) {
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil || path == string(filepath.Separator) {
		return root, err
	}
	defer func() { _ = root.Close() }()
	return openDir(root, strings.TrimPrefix(path, string(filepath.Separator)), false)
}

func readNodes(root *os.Root, prefix string, result *tree) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("reading %s: %w", root.Name(), err)
	}
	for _, entry := range entries {
		name := entry.Name()
		n := node{path: filepath.Join(prefix, name)}
		if entry.IsDir() {
			child, err := openChild(root, name, false)
			if err != nil {
				return err
			}
			n.mode = fs.ModeDir
			err = readNodes(child, n.path, result)
			_ = child.Close()
			if err != nil {
				return err
			}
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file or directory", filepath.Join(root.Name(), name))
			}
			n.mode = info.Mode()
			n.data, err = readRegular(root, name, info)
			if err != nil {
				return fmt.Errorf("reading %s: %w", filepath.Join(root.Name(), name), err)
			}
		}
		*result = append(*result, n)
	}
	return nil
}

func readRegular(root *os.Root, name string, expected fs.FileInfo) ([]byte, error) {
	// Nonblocking open keeps a FIFO swapped in after lstat from hanging a run.
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		return nil, errors.New("file changed while opening it")
	}
	return io.ReadAll(f)
}

func (t tree) digest() string {
	h := sha256.New()
	var length [8]byte
	for _, n := range t {
		path := filepath.ToSlash(n.path)
		kind := byte('f')
		if n.mode.IsDir() {
			kind = 'd'
		}
		_, _ = h.Write([]byte{kind})
		binary.BigEndian.PutUint64(length[:], uint64(len(path)))
		_, _ = h.Write(length[:])
		_, _ = io.WriteString(h, path)
		binary.BigEndian.PutUint64(length[:], uint64(len(n.data)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(n.data)
	}
	return hex.EncodeToString(h.Sum(nil))
}
