//go:build !linux && !darwin

package problem

import "errors"

func installArtifactNoReplace(_ int, _, _ string) error {
	return errors.New("atomic no-replace artifact installation is unsupported on this platform")
}
