package autoscaler

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVMSize(t *testing.T) {
	size := VMSize{CPU: 4, MemoryGiB: 8}
	assert.Equal(t, 4, size.CPU)
	assert.Equal(t, 8, size.MemoryGiB)
}

func TestAtoiDefault(t *testing.T) {
	assert.Equal(t, 5, atoiDefault("5", 0))
	assert.Equal(t, 0, atoiDefault("", 0))
	assert.Equal(t, 3, atoiDefault("abc", 3))
}
