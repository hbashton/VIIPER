package cpp

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/internal/codegen/meta"
	"github.com/Alia5/VIIPER/internal/codegen/scanner"
)

func TestRequiredXboxOneChildrenUseMemberJSONSerializer(t *testing.T) {
	dtos, err := scanner.ScanDTOs(filepath.Join("..", "..", "..", "..", "viipertypes", "structs.go"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := generateTypes(logger, directory, &meta.Metadata{DTOs: dtos}); err != nil {
		t.Fatal(err)
	}
	header, err := os.ReadFile(filepath.Join(directory, "types.hpp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"identity", "usb", "strings", "feedback"} {
		want := `j["` + name + `"] = ` + name + `.to_json();`
		if !strings.Contains(string(header), want) {
			t.Errorf("required Xbox One child lacks member serialization: %s", want)
		}
	}
}

func TestNamedDTOJSONPreservesOptionalPointerArrayAndPrimitivePaths(t *testing.T) {
	directory, header := generateNamedDTOFixture(t)
	for _, want := range []string{
		`ParentDTO result{};`,
		`j["required"] = required.to_json();`,
		`j["optional"] = optional.value().to_json();`,
		`j["pointer"] = pointer.value().to_json();`,
		`std::optional<NestedDTO> pointer;`,
		`result.pointer = NestedDTO::from_json(j["pointer"]);`,
		`result.pointer = std::nullopt;`,
		`arr.push_back(item.to_json());`,
		`j["label"] = label;`,
		`arr.push_back(item);`,
		`j["properties"] = properties;`,
	} {
		if !strings.Contains(header, want) {
			t.Errorf("generated DTO contract lacks %s", want)
		}
	}
	// Opt-in native check uses an explicitly supplied, existing JSON dependency.
	// Ordinary Go unit tests remain independent of a C++ toolchain or downloads.
	jsonInclude := os.Getenv("VIIPER_CPP_JSON_INCLUDE")
	if jsonInclude == "" {
		return
	}
	compiler := os.Getenv("CXX")
	if compiler == "" {
		compiler = "c++"
	}
	source := filepath.Join(directory, "roundtrip.cpp")
	if err := os.WriteFile(source, []byte(namedDTORoundTrip), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "roundtrip.exe")
	command := exec.Command(compiler, "-std=c++20", "-Wall", "-Wextra", "-Werror", "-I", jsonInclude,
		"-I", directory, source, "-o", binary)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile generated DTOs: %v\n%s", err, output)
	}
	if output, err := exec.Command(binary).CombinedOutput(); err != nil {
		t.Fatalf("generated DTO round trip: %v\n%s", err, output)
	}
}

func generateNamedDTOFixture(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	fixture := filepath.Join(directory, "fixture.go")
	source := "package fixture\n" +
		"type NestedDTO struct { Value uint32 `json:\"value\"` }\n" +
		"type ParentDTO struct {\n" +
		"Required NestedDTO `json:\"required\"`\n" +
		"Optional NestedDTO `json:\"optional,omitempty\"`\n" +
		"Pointer *NestedDTO `json:\"pointer,omitempty\"`\n" +
		"Items []NestedDTO `json:\"items\"`\n" +
		"OptionalItems []NestedDTO `json:\"optionalItems,omitempty\"`\n" +
		"Label string `json:\"label\"`\n" +
		"Labels []string `json:\"labels\"`\n" +
		"Properties map[string]string `json:\"properties\"`\n}\n"
	if err := os.WriteFile(fixture, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	dtos, err := scanner.ScanDTOs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	productionDTOs, err := scanner.ScanDTOs(filepath.Join("..", "..", "..", "..", "viipertypes", "structs.go"))
	if err != nil {
		t.Fatal(err)
	}
	dtos = append(productionDTOs, dtos...)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	detailDirectory := filepath.Join(directory, "detail")
	if err := os.Mkdir(detailDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, generate := range []func() error{
		func() error { return generateConfig(logger, directory) },
		func() error { return generateError(logger, directory) },
		func() error { return generateJSON(logger, detailDirectory) },
		func() error { return generateTypes(logger, directory, &meta.Metadata{DTOs: dtos}) },
	} {
		if err := generate(); err != nil {
			t.Fatal(err)
		}
	}
	header, err := os.ReadFile(filepath.Join(directory, "types.hpp"))
	if err != nil {
		t.Fatal(err)
	}
	return directory, string(header)
}

const namedDTORoundTrip = `
#define VIIPER_JSON_INCLUDE <nlohmann/json.hpp>
#define VIIPER_JSON_NAMESPACE nlohmann
#define VIIPER_JSON_TYPE json
#include "types.hpp"
#include <cassert>

int main() {
    auto input = nlohmann::json::parse(R"({"required":{"value":7},"optional":{"value":8},"pointer":{"value":9},"items":[{"value":10}],"optionalItems":[{"value":11}],"label":"aim","labels":["left","right"],"properties":{"mode":"native"}})");
    auto parent = viiper::ParentDTO::from_json(input);
    assert(parent.to_json() == input);
    parent.optional.reset();
    parent.pointer.reset();
    parent.optionalitems.reset();
    auto absent = parent.to_json();
    assert(!absent.contains("optional"));
    assert(!absent.contains("pointer"));
    assert(!absent.contains("optionalItems"));
    absent["pointer"] = nullptr;
    auto restored = viiper::ParentDTO::from_json(absent);
    assert(!restored.pointer.has_value());
    assert(restored.required.value == 7);
    assert(restored.items.at(0).value == 10);
    auto missing = viiper::ParentDTO::from_json(nlohmann::json::object());
    assert(missing.required.value == 0);
    assert(missing.to_json()["required"]["value"] == 0);
}
`
