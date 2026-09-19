package archive_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/utkuozdemir/pv-migrate/internal/archive"
)

func TestInterpret(t *testing.T) {
	t.Parallel()

	assert.Empty(t, archive.Interpret(0))
	assert.Contains(t, archive.Interpret(1), "changed while tar was reading")
	assert.Contains(t, archive.Interpret(2), "fatal")
	assert.Empty(t, archive.Interpret(64), "an undocumented code gets no invented meaning")
}
