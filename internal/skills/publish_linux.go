package skills

import "golang.org/x/sys/unix"

func publish(fd int, temporary, name string) error {
	return unix.Renameat2(fd, temporary, fd, name, unix.RENAME_NOREPLACE)
}
