package skills

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
)

func capture(parent *os.Root, name string, content tree) error {
	temporary := ".agentsync-skill-" + rand.Text()
	if err := parent.Mkdir(temporary, 0o700); err != nil {
		return err
	}
	defer func() { _ = parent.RemoveAll(temporary) }()
	staged, err := openChild(parent, temporary, false)
	if err != nil {
		return err
	}
	defer func() { _ = staged.Close() }()
	for _, n := range content {
		if err := writeNode(staged, n); err != nil {
			return fmt.Errorf("preparing %s: %w", n.path, err)
		}
	}
	f, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// The exclusive rename is the destination reservation and publication.
	// Even an empty destination created by a concurrent process wins.
	return publish(int(f.Fd()), temporary, name)
}

func writeNode(root *os.Root, n node) error {
	if n.mode.IsDir() {
		return root.Mkdir(n.path, 0o755)
	}
	mode := 0o644 | (n.mode.Perm() & 0o111)
	f, err := root.OpenFile(n.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(n.data)
	return errors.Join(err, f.Chmod(mode), f.Close())
}
