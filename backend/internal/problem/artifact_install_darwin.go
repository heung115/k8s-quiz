//go:build darwin

package problem

import "golang.org/x/sys/unix"

func installArtifactNoReplace(dirFD int, oldName, newName string) error {
	return unix.RenameatxNp(dirFD, oldName, dirFD, newName, unix.RENAME_EXCL)
}
