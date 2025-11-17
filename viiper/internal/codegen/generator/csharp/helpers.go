package csharp

import (
	"strings"
	"viiper/internal/codegen/common"
)

func toPascalCase(s string) string {
	return common.ToPascalCase(s)
}

func toCamelCase(s string) string {
	return common.ToCamelCase(s)
}

func goTypeToCSharp(goType string) string {
	goType = strings.TrimPrefix(goType, "*")
	goType = strings.TrimPrefix(goType, "[]")

	switch goType {
	case "uint8":
		return "byte"
	case "uint16":
		return "ushort"
	case "uint32":
		return "uint"
	case "uint64":
		return "ulong"
	case "int8":
		return "sbyte"
	case "int16":
		return "short"
	case "int32", "int":
		return "int"
	case "int64":
		return "long"
	case "float32":
		return "float"
	case "float64":
		return "double"
	case "bool":
		return "bool"
	case "string":
		return "string"
	case "byte":
		return "byte"
	default:
		return toPascalCase(goType)
	}
}

func writeFileHeader() string {
	return `// Auto-generated VIIPER C# SDK
// DO NOT EDIT - This file is generated from the VIIPER server codebase

`
}
