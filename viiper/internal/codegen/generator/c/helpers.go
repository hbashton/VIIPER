package cgen

import (
	"fmt"
	"strings"
	"text/template"
	"viiper/internal/codegen/common"
	"viiper/internal/codegen/meta"
	"viiper/internal/codegen/scanner"
)

func tplFuncs(md *meta.Metadata) template.FuncMap {
	return template.FuncMap{
		"ctype":       cType,
		"snakecase":   common.ToSnakeCase,
		"upper":       strings.ToUpper,
		"hasWireTag":  func(device, direction string) bool { return hasWireTag(md, device, direction) },
		"wireFields":  func(device, direction string) string { return wireFields(md, device, direction) },
		"indent":      indent,
		"fieldDecl":   fieldDecl,
		"pathParams":  orderedPathParams,
		"join":        strings.Join,
		"mapFuncDecl": mapFuncDecl,
		"mapFuncImpl": mapFuncImpl,
	}
}

func cType(goType, kind string) string {
	switch {
	case strings.HasPrefix(goType, "[]"):
		elem := strings.TrimPrefix(goType, "[]")
		return cType(elem, "")
	}

	switch goType {
	case "string":
		return "const char*"
	case "uint8":
		return "uint8_t"
	case "uint16":
		return "uint16_t"
	case "uint32":
		return "uint32_t"
	case "uint64":
		return "uint64_t"
	case "int8":
		return "int8_t"
	case "int16":
		return "int16_t"
	case "int32":
		return "int32_t"
	case "int64":
		return "int64_t"
	case "bool":
		return "int"
	case "float32":
		return "float"
	case "float64":
		return "double"
	default:
		if goType == "Device" {
			return "viiper_device_info_t"
		}
		if kind == "struct" {
			return fmt.Sprintf("viiper_%s_t*", common.ToSnakeCase(goType))
		}
		return goType
	}
}

func hasWireTag(md *meta.Metadata, device, direction string) bool {
	if md.WireTags == nil {
		return false
	}
	return md.WireTags.HasDirection(device, direction)
}

func wireFields(md *meta.Metadata, device, direction string) string {
	if md.WireTags == nil {
		return "/* no wire tags found */"
	}

	tag := md.WireTags.GetTag(device, direction)
	if tag == nil {
		return "/* no wire tag for this device/direction */"
	}

	return renderCWireFields(tag)
}

func renderCWireFields(tag *scanner.WireTag) string {
	if tag == nil || len(tag.Fields) == 0 {
		return "/* no fields */"
	}

	var lines []string
	for _, field := range tag.Fields {
		if strings.Contains(field.Type, "*") {
			base := strings.Split(field.Type, "*")[0]
			cbase := wireBaseToC(base)
			lines = append(lines, fmt.Sprintf("%s* %s; size_t %s_count;", cbase, field.Name, common.ToSnakeCase(field.Name)))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s;", wireBaseToC(field.Type), field.Name))
	}
	return strings.Join(lines, "\n    ")
}

func wireBaseToC(wireType string) string {
	switch wireType {
	case "u8":
		return "uint8_t"
	case "u16":
		return "uint16_t"
	case "u32":
		return "uint32_t"
	case "u64":
		return "uint64_t"
	case "i8":
		return "int8_t"
	case "i16":
		return "int16_t"
	case "i32":
		return "int32_t"
	case "i64":
		return "int64_t"
	default:
		return wireType
	}
}

func indent(spaces int, s string) string {
	prefix := strings.Repeat(" ", spaces)
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		if p != "" {
			parts[i] = prefix + p
		}
	}
	return strings.Join(parts, "\n")
}

func orderedPathParams(path string) []string {
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	var params []string
	for _, p := range parts {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			params = append(params, p[1:len(p)-1])
		}
	}
	return params
}

func fieldDecl(f scanner.FieldInfo) string {
	if f.TypeKind == "slice" || strings.HasPrefix(f.Type, "[]") {
		elem := strings.TrimPrefix(f.Type, "[]")
		cElem := cType(elem, "")
		return fmt.Sprintf("%s* %s; size_t %s_count;%s", cElem, f.Name, common.ToSnakeCase(f.Name), optComment(f))
	}
	return fmt.Sprintf("%s %s;%s", cType(f.Type, f.TypeKind), f.Name, optComment(f))
}

func optComment(f scanner.FieldInfo) string {
	if f.Optional {
		return " /* optional */"
	}
	return ""
}

func mapFuncDecl(device string, mapInfo scanner.MapInfo) string {
	keyType := mapGoTypeToCType(mapInfo.KeyType)
	valueType := mapGoTypeToCType(mapInfo.ValueType)
	funcName := fmt.Sprintf("viiper_%s_%s_lookup", device, common.ToSnakeCase(mapInfo.Name))

	return fmt.Sprintf("int %s(%s key, %s* out_value);", funcName, keyType, valueType)
}

func mapFuncImpl(device string, mapInfo scanner.MapInfo) string {
	keyType := mapGoTypeToCType(mapInfo.KeyType)
	valueType := mapGoTypeToCType(mapInfo.ValueType)
	funcName := fmt.Sprintf("viiper_%s_%s_lookup", device, common.ToSnakeCase(mapInfo.Name))

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("int %s(%s key, %s* out_value) {\n", funcName, keyType, valueType))
	builder.WriteString("    if (!out_value) return 0;\n")
	builder.WriteString("    switch (key) {\n")

	keys := common.SortedStringKeys(mapInfo.Entries)
	for _, keyStr := range keys {
		value := mapInfo.Entries[keyStr]
		cKey := formatCMapKey(keyStr, mapInfo.KeyType, device)
		cValue := formatCMapValue(value, mapInfo.ValueType, device)
		builder.WriteString(fmt.Sprintf("        case %s: *out_value = %s; return 1;\n", cKey, cValue))
	}

	builder.WriteString("        default: return 0;\n")
	builder.WriteString("    }\n")
	builder.WriteString("}\n")

	return builder.String()
}

func mapGoTypeToCType(goType string) string {
	switch goType {
	case "byte", "uint8":
		return "uint8_t"
	case "uint16":
		return "uint16_t"
	case "uint32", "uint":
		return "uint32_t"
	case "uint64":
		return "uint64_t"
	case "int8":
		return "int8_t"
	case "int16":
		return "int16_t"
	case "int32", "int":
		return "int32_t"
	case "int64":
		return "int64_t"
	case "bool":
		return "int"
	case "string":
		return "const char*"
	default:
		return goType
	}
}

func formatCMapKey(key string, goType string, device string) string {
	switch goType {
	case "byte", "uint8":
		if len(key) == 1 {
			// Escape special characters
			switch key[0] {
			case '\n':
				return "'\\n'"
			case '\r':
				return "'\\r'"
			case '\t':
				return "'\\t'"
			case '\\':
				return "'\\\\'"
			case '\'':
				return "'\\''"
			case 0:
				return "'\\0'"
			}
			if key[0] >= 32 && key[0] <= 126 {
				return fmt.Sprintf("'%s'", key)
			}
			return fmt.Sprintf("0x%02X", key[0])
		}
		if len(key) > 0 && (key[0] >= 'A' && key[0] <= 'Z') {
			prefix := common.ExtractPrefix(key)
			if prefix != "" {
				constName := strings.ToUpper(key)
				return fmt.Sprintf("VIIPER_%s_%s", strings.ToUpper(device), constName)
			}
		}
		return key
	case "string":
		return fmt.Sprintf("\"%s\"", key)
	default:
		return key
	}
}

func formatCMapValue(value interface{}, goType string, device string) string {
	switch goType {
	case "byte", "uint8", "uint16", "uint32", "uint64":
		if str, ok := value.(string); ok && !strings.Contains(str, " ") {
			if len(str) > 0 && (str[0] >= 'A' && str[0] <= 'Z') {
				prefix := common.ExtractPrefix(str)
				if prefix != "" {
					constName := strings.ToUpper(str)
					return fmt.Sprintf("VIIPER_%s_%s", strings.ToUpper(device), constName)
				}
			}
			return str
		}
		if num, ok := value.(int64); ok {
			return fmt.Sprintf("0x%X", num)
		}
		if num, ok := value.(uint64); ok {
			return fmt.Sprintf("0x%X", num)
		}
		return fmt.Sprintf("%v", value)
	case "bool":
		if b, ok := value.(bool); ok {
			if b {
				return "1"
			}
			return "0"
		}
		if str, ok := value.(string); ok {
			if str == "true" {
				return "1"
			}
			if str == "false" {
				return "0"
			}
			return str
		}
		return "0"
	case "string":
		if str, ok := value.(string); ok {
			return fmt.Sprintf("\"%s\"", str)
		}
		return fmt.Sprintf("\"%v\"", value)
	default:
		return fmt.Sprintf("%v", value)
	}
}
