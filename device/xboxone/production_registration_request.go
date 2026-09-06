package xboxone

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var ErrProductionRegistrationRequest = errors.New("xboxone: invalid production registration request")

// DecodeProductionRegistrationRequest decodes the shared, bounded management
// capability envelope. Never return payload fragments in errors or put this
// capability in a route, descriptor, device listing, or log.
func DecodeProductionRegistrationRequest(payload string) (string, error) {
	invalid := ErrProductionRegistrationRequest
	if len(payload) == 0 || len(payload) > 256 {
		return "", invalid
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return "", invalid
	}
	var version uint16
	var token string
	var versionSeen, tokenSeen bool
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", invalid
		}
		switch key {
		case "version":
			if versionSeen || decoder.Decode(&version) != nil {
				return "", invalid
			}
			versionSeen = true
		case "removalToken":
			if tokenSeen || decoder.Decode(&token) != nil {
				return "", invalid
			}
			tokenSeen = true
		default:
			return "", invalid
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || !versionSeen || !tokenSeen ||
		version != 1 || !ValidProductionRemovalToken(token) {
		return "", invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", invalid
	}
	return token, nil
}
