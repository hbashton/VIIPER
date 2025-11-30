package common

import (
	"strings"

	"github.com/Alia5/VIIPER/internal/codegen/scanner"
)

// WireTypeSize returns the size in bytes of a wire protocol type.
func WireTypeSize(wireType string) int {
	switch wireType {
	case "u8", "i8":
		return 1
	case "u16", "i16":
		return 2
	case "u32", "i32":
		return 4
	case "u64", "i64":
		return 8
	default:
		return 1
	}
}

// CalculateOutputSize computes the exact size in bytes of a device's output (s2c) message.
// Returns 0 if the tag is nil or device has no output.
// For variable-length fields (e.g., "u8*count"), returns 0 to indicate dynamic size.
func CalculateOutputSize(tag *scanner.WireTag) int {
	if tag == nil {
		return 0
	}

	total := 0
	for _, field := range tag.Fields {
		baseType := field.Type
		if strings.Contains(baseType, "*") {
			return 0
		}
		total += WireTypeSize(baseType)
	}

	return total
}
