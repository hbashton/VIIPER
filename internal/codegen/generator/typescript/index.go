package typescript

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/template"

	"github.com/Alia5/VIIPER/internal/codegen/common"
	"github.com/Alia5/VIIPER/internal/codegen/meta"
)

const indexTemplate = `{{writeFileHeaderTS}}
export * from './ViiperClient';
export * from './ViiperDevice';
export * as Types from './types/ManagementDtos';
export * as Keyboard from './devices/Keyboard';
export * as Mouse from './devices/Mouse';
export * as Xbox360 from './devices/Xbox360';
`

const deviceIndexTemplate = `{{writeFileHeaderTS}}
{{if .HasInput}}export * from './{{.PascalName}}Input';
{{end}}
{{if .HasOutput}}export * from './{{.PascalName}}Output';
{{end}}export * from './{{.PascalName}}Constants';
{{if .HasMeta}}export * from './{{.PascalName}}Meta';
{{end}}
`

func generateIndex(logger *slog.Logger, srcDir string) error {
	logger.Debug("Generating index.ts re-exports")
	f, err := os.Create(filepath.Join(srcDir, "index.ts"))
	if err != nil {
		return fmt.Errorf("write index.ts: %w", err)
	}
	defer f.Close() //nolint:errcheck
	tmpl := template.Must(template.New("index").Funcs(template.FuncMap{
		"writeFileHeaderTS": writeFileHeaderTS,
	}).Parse(indexTemplate))
	if err := tmpl.Execute(f, nil); err != nil {
		return fmt.Errorf("execute index template: %w", err)
	}
	return nil
}

func generateDeviceIndex(logger *slog.Logger, deviceDir, deviceName string, md *meta.Metadata) error {
	logger.Debug("Generating device index.ts", "device", deviceName)

	pascalName := common.ToPascalCase(deviceName)

	// Use the same metadata as the emitters. Stale files from an earlier
	// generation must not advertise a wire protocol that no longer exists.
	hasInput := md.WireTags != nil && md.WireTags.GetTag(deviceName, "c2s") != nil
	hasOutput := md.WireTags != nil && md.WireTags.GetTag(deviceName, "s2c") != nil
	hasMeta := len(md.DeviceStructs[deviceName]) != 0

	f, err := os.Create(filepath.Join(deviceDir, "index.ts"))
	if err != nil {
		return fmt.Errorf("write device index.ts: %w", err)
	}
	defer f.Close() //nolint:errcheck

	tmpl := template.Must(template.New("deviceIndex").Funcs(template.FuncMap{
		"writeFileHeaderTS": writeFileHeaderTS,
	}).Parse(deviceIndexTemplate))

	data := struct {
		PascalName string
		HasInput   bool
		HasOutput  bool
		HasMeta    bool
	}{
		PascalName: pascalName,
		HasInput:   hasInput,
		HasOutput:  hasOutput,
		HasMeta:    hasMeta,
	}

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("execute device index template: %w", err)
	}
	return nil
}
