package common

import (
	"strings"
	"unicode"
)

func ToPascalCase(s string) string {
	if s == "" {
		return ""
	}

	words := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || unicode.IsSpace(r)
	})

	var result strings.Builder
	for _, word := range words {
		if len(word) > 0 {
			result.WriteString(strings.ToUpper(string(word[0])))
			if len(word) > 1 {
				result.WriteString(strings.ToLower(word[1:]))
			}
		}
	}

	return result.String()
}

func ToCamelCase(s string) string {
	pascal := ToPascalCase(s)
	if len(pascal) == 0 {
		return ""
	}
	return strings.ToLower(string(pascal[0])) + pascal[1:]
}

func ToSnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// ExtractPrefix extracts the common prefix from a constant name for enum grouping
// Examples: "Key_A" -> "Key_", "ModifierShift" -> "Modifier", "LED1" -> "LED"
func ExtractPrefix(name string) string {
	if idx := strings.IndexRune(name, '_'); idx >= 0 {
		return name[:idx+1]
	}

	if len(name) > 1 && isUpper(name[0]) {
		runEnd := 1
		for runEnd < len(name) && isUpper(name[runEnd]) {
			runEnd++
		}
		if runEnd < len(name) && isLower(name[runEnd]) && runEnd > 1 {
			return name[:runEnd-1]
		}
		if runEnd > 1 {
			return name[:runEnd]
		}
	}

	for i := 1; i < len(name); i++ {
		if name[i] >= '0' && name[i] <= '9' {
			return name[:i]
		}
		if isUpper(name[i]) && isLower(name[i-1]) {
			return name[:i]
		}
	}

	if (name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z') {
		return name
	}
	return ""
}

func isUpper(b byte) bool { return b >= 'A' && b <= 'Z' }
func isLower(b byte) bool { return b >= 'a' && b <= 'z' }
