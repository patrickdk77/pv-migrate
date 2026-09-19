package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A tar job's code is read with tar's table, not rclone's: the two disagree
// on what 2 means.
func TestInterpretExitCode_TarJob(t *testing.T) {
	t.Parallel()

	tar := interpretExitCode("pv-migrate-x-backup-tar", 2)
	assert.Contains(t, tar, "fatal")
	assert.NotEqual(t, interpretExitCode("pv-migrate-x-backup-rclone", 2), tar,
		"a tar job must not be read with rclone's table")
}
