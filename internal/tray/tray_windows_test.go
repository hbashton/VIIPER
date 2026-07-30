//go:build windows

package tray

import (
	"testing"

	"github.com/Alia5/VIIPER/internal/codegen/common"
	"github.com/stretchr/testify/require"
)

func TestReadVersionPrefersInjectedReleaseVersion(t *testing.T) {
	original := common.Version
	common.Version = "v0.0.6"
	t.Cleanup(func() { common.Version = original })

	require.Equal(t, "0.0.6", readVersion())
}
