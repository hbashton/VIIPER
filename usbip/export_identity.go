package usbip

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

const ProductionXboxOneBusIDLength = 29

var errInvalidExportBusID = errors.New("invalid USB/IP export bus ID")
var productionXboxOneBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewProductionXboxOneBusID creates a public per-registration export identity,
// independent of any API authentication/removal secret. Never derive it from
// numeric addresses or counters, which can recur after removal or restart.
func NewProductionXboxOneBusID() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return productionXboxOneBusIDFromEntropy(entropy), nil
}

func productionXboxOneBusIDFromEntropy(entropy [16]byte) string {
	return "x1-" + strings.ToLower(productionXboxOneBase32.EncodeToString(entropy[:]))
}

func ValidProductionXboxOneBusID(value string) bool {
	if len(value) != ProductionXboxOneBusIDLength || !strings.HasPrefix(value, "x1-") {
		return false
	}
	for _, value := range value[3:] {
		if (value < 'a' || value > 'z') && (value < '2' || value > '7') {
			return false
		}
	}
	// 128 bits occupy 26 base32 characters. The final two unused bits must
	// be zero; reject noncanonical aliases for the same decoded byte string.
	return strings.ContainsRune("aeimquy4", rune(value[len(value)-1]))
}

// ExportBusID reads the actual immutable export identity. It never rebuilds
// missing metadata from numeric fields: doing so would discard an incarnation
// alias and permit a reconnect to select an unrelated successor.
func ExportBusID(meta ExportMeta) (string, error) {
	end := bytes.IndexByte(meta.USBBusID[:], 0)
	if end <= 0 {
		return "", errInvalidExportBusID
	}
	for _, value := range meta.USBBusID[end:] {
		if value != 0 {
			return "", errInvalidExportBusID
		}
	}
	value := string(meta.USBBusID[:end])
	if ValidProductionXboxOneBusID(value) || value == fmt.Sprintf("%d-%d", meta.BusID, meta.DevID) {
		return value, nil
	}
	return "", errInvalidExportBusID
}
