package usbip

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionXboxOneBusIDGoldenEncodingAndCanonicalValidation(t *testing.T) {
	var zero, ones [16]byte
	for i := range ones {
		ones[i] = 0xff
	}
	require.Equal(t, "x1-"+strings.Repeat("a", 26), productionXboxOneBusIDFromEntropy(zero))
	require.Equal(t, "x1-"+strings.Repeat("7", 25)+"4", productionXboxOneBusIDFromEntropy(ones))
	for _, last := range "aeimquy4" {
		require.True(t, ValidProductionXboxOneBusID("x1-"+strings.Repeat("a", 25)+string(last)))
	}
	valid := productionXboxOneBusIDFromEntropy(zero)
	for _, invalid := range []string{"", "17-4", strings.ToUpper(valid), valid[:28], valid + "a",
		"x2-" + valid[3:], "x1-" + strings.Repeat("a", 25) + "b", "x1-" + strings.Repeat("a", 25) + "7",
		"x1-" + strings.Repeat("a", 24) + "0a", "x1-" + strings.Repeat("a", 24) + "=a", valid[:28] + "\x00"} {
		require.False(t, ValidProductionXboxOneBusID(invalid), "accepted %q", invalid)
	}
	seen := make(map[string]bool)
	for range 64 {
		alias, err := NewProductionXboxOneBusID()
		require.NoError(t, err)
		require.True(t, ValidProductionXboxOneBusID(alias))
		require.False(t, seen[alias])
		seen[alias] = true
	}
}

func TestExportBusIDDoesNotReconstructMissingOrMalformedMetadata(t *testing.T) {
	meta := ExportMeta{BusID: 17, DevID: 4}
	_, err := ExportBusID(meta)
	require.Error(t, err)
	copy(meta.USBBusID[:], "17-4")
	actual, err := ExportBusID(meta)
	require.NoError(t, err)
	require.Equal(t, "17-4", actual)
	meta.DevID = 5
	_, err = ExportBusID(meta)
	require.Error(t, err)
	alias := "x1-" + strings.Repeat("a", 26)
	meta.USBBusID = [32]byte{}
	copy(meta.USBBusID[:], alias)
	actual, err = ExportBusID(meta)
	require.NoError(t, err)
	require.Equal(t, alias, actual)
	meta.USBBusID[31] = 'a'
	_, err = ExportBusID(meta)
	require.Error(t, err)
}

func TestProductionAliasWireGoldenPreservesNumericTransferIdentity(t *testing.T) {
	device := ExportedDevice{ExportMeta: ExportMeta{BusID: 0x1234, DevID: 0x5678}}
	alias := "x1-" + strings.Repeat("a", 26)
	copy(device.USBBusID[:], alias)
	for _, write := range []func(*bytes.Buffer) error{
		func(dst *bytes.Buffer) error { return device.WriteDevlist(dst) },
		func(dst *bytes.Buffer) error { return device.WriteImport(dst) },
	} {
		var buffer bytes.Buffer
		require.NoError(t, write(&buffer))
		wire := buffer.Bytes()
		require.Equal(t, alias, string(wire[256:285]))
		require.Equal(t, []byte{0, 0, 0}, wire[285:288])
		require.Equal(t, uint32(0x1234), binary.BigEndian.Uint32(wire[288:292]))
		require.Equal(t, uint32(0x5678), binary.BigEndian.Uint32(wire[292:296]))
	}
}
