package typescript

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

func TestDeviceIndexExportsOnlyCurrentMetadata(t *testing.T) {
	for _, withInput := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed-protocol", true: "tagged-input"}[withInput], func(t *testing.T) {
			dir := t.TempDir()
			// Deliberately stale output/meta files cannot resurrect exports.
			for _, file := range []string{"XboxoneInput.ts", "XboxoneOutput.ts", "XboxoneMeta.ts"} {
				if err := os.WriteFile(filepath.Join(dir, file), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			md := &meta.Metadata{}
			if withInput {
				md.WireTags = &scanner.WireTags{Tags: map[string]map[string]*scanner.WireTag{
					"xboxone": {"c2s": {Device: "xboxone", Direction: "c2s"}},
				}}
			}
			if err := generateDeviceIndex(slog.New(slog.NewTextHandler(io.Discard, nil)), dir, "xboxone", md); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "index.ts"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "XboxoneInput") != withInput ||
				strings.Contains(string(data), "XboxoneOutput") || strings.Contains(string(data), "XboxoneMeta") ||
				!strings.Contains(string(data), "XboxoneConstants") {
				t.Fatalf("exports do not match current metadata: %s", data)
			}
		})
	}
}
