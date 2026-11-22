package skills

import "golang.org/x/sys/unix"

func publish(fd int, temporary, name string) error {
	return unix.RenameatxNp(fd, temporary, fd, name, unix.RENAME_EXCL)
}
