//go:build linux

package problem

import "golang.org/x/sys/unix"

func installArtifactNoReplace(dirFD int, oldName, newName string) error {
	return unix.Renameat2(dirFD, oldName, dirFD, newName, uint(unix.RENAME_NOREPLACE))
}
