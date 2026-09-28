package auth

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"unicode"
	"unicode/utf8"

	"github.com/100bench/OPORA/internal/domain"
)

// ErrInvalidConfiguration reports an unusable device-token map without echoing it.
var ErrInvalidConfiguration = errors.New("device token configuration is invalid")

// ErrUnauthorized reports a bearer token that did not match any configured device.
var ErrUnauthorized = errors.New("device token is invalid")

type tokenEntry struct {
	deviceID string
	hash     [sha256.Size]byte
}

// DeviceTokens stores only fixed-size hashes of configured bearer tokens.
type DeviceTokens struct {
	entries []tokenEntry
}

// NewDeviceTokens parses a JSON device-to-token mapping.
func NewDeviceTokens(raw string) (*DeviceTokens, error) {
	if !utf8.ValidString(raw) {
		return nil, ErrInvalidConfiguration
	}
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrInvalidConfiguration
	}
	seenDevices := make(map[string]struct{})
	seenTokens := make(map[[sha256.Size]byte]struct{})
	entries := make([]tokenEntry, 0)
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, ErrInvalidConfiguration
		}
		deviceID, ok := keyToken.(string)
		if !ok || !validCredentialPart(deviceID, 128) {
			return nil, ErrInvalidConfiguration
		}
		if _, exists := seenDevices[deviceID]; exists {
			return nil, ErrInvalidConfiguration
		}
		var token string
		if err := dec.Decode(&token); err != nil || !validCredentialPart(token, 1024) {
			return nil, ErrInvalidConfiguration
		}
		hash := sha256.Sum256([]byte(token))
		if _, exists := seenTokens[hash]; exists {
			return nil, ErrInvalidConfiguration
		}
		seenDevices[deviceID] = struct{}{}
		seenTokens[hash] = struct{}{}
		entries = append(entries, tokenEntry{deviceID: deviceID, hash: hash})
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') || len(entries) == 0 {
		return nil, ErrInvalidConfiguration
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidConfiguration
	}
	return &DeviceTokens{entries: entries}, nil
}

func validCredentialPart(value string, maxLength int) bool {
	count := utf8.RuneCountInString(value)
	if count < 1 || count > maxLength {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Authenticate resolves a token with fixed-size constant-time comparisons.
func (a *DeviceTokens) Authenticate(token string) (domain.Device, error) {
	if a == nil || token == "" {
		return domain.Device{}, ErrUnauthorized
	}
	candidate := sha256.Sum256([]byte(token))
	match := -1
	for i := range a.entries {
		if subtle.ConstantTimeCompare(candidate[:], a.entries[i].hash[:]) == 1 {
			match = i
		}
	}
	if match < 0 {
		return domain.Device{}, ErrUnauthorized
	}
	return domain.Device{ID: a.entries[match].deviceID}, nil
}
