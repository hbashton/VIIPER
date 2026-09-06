package rust

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/internal/codegen/meta"
	"github.com/Alia5/VIIPER/internal/codegen/scanner"
)

func TestNamedConstantsUseDeclaredScalarWidth(t *testing.T) {
	dir := t.TempDir()
	md := &meta.Metadata{DevicePackages: map[string]*scanner.DeviceConstants{
		"fixture": {Constants: []scanner.ConstantInfo{
			{Name: "Flag", Type: "Flags", UnderlyingType: "uint64", Value: uint64(1)},
			{Name: "Mode", Type: "Modes", UnderlyingType: "uint8", Value: uint64(1)},
			{Name: "NumericString", Type: "Text", UnderlyingType: "string", Value: "42"},
		}},
	}}
	if err := generateConstants(slog.New(slog.NewTextHandler(io.Discard, nil)), dir, "fixture", md); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "constants.rs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "FLAG: u64 = 1;") || !strings.Contains(string(data), "MODE: u8 = 1;") || strings.Contains(string(data), "NUMERIC_STRING") {
		t.Fatalf("incorrect Rust scalar declarations: %s", data)
	}
}
