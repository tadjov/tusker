package version

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestToolOf_AppendsTheBuildRevision(t *testing.T) {
	const revision = "765ebc56a1b2c3d4e5f60718293a4b5c6d7e8f90"
	tests := []struct {
		name     string
		info     *debug.BuildInfo
		expected string
	}{
		{name: "no build info", info: nil, expected: "bench/1.0.0"},
		{name: "no vcs settings", info: &debug.BuildInfo{}, expected: "bench/1.0.0"},
		{
			name:     "clean checkout",
			info:     &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: "false"}}},
			expected: "bench/1.0.0+765ebc56a1b2",
		},
		{
			name:     "modified checkout",
			info:     &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: "true"}}},
			expected: "bench/1.0.0+765ebc56a1b2-modified",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, toolOf(tc.info))
		})
	}
}
