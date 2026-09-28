package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestDeviceTokensResolveDeviceWithoutRetainingPlainToken(t *testing.T) {
	a, err := NewDeviceTokens(`{"device-a":"token-a","device-b":"token-b"}`)
	if err != nil {
		t.Fatal(err)
	}
	device, err := a.Authenticate("token-b")
	if err != nil || device.ID != "device-b" {
		t.Fatalf("device=%#v err=%v", device, err)
	}
	if _, err := a.Authenticate("missing"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err=%v", err)
	}
	for _, entry := range a.entries {
		if strings.Contains(string(entry.hash[:]), "token-") {
			t.Fatal("plain token retained")
		}
	}
}

func TestDeviceTokensRejectUnsafeConfigurationWithoutEchoingIt(t *testing.T) {
	secret := "very-secret-token"
	invalid := []string{
		``,
		`{}`,
		`{"a":"same","b":"same"}`,
		`{"a":"one","a":"two"}`,
		`{"bad device":"token"}`,
		`{"a":"bad token"}`,
		`{"a":null}`,
		`{"a":"` + secret + `"} trailing`,
	}
	for _, raw := range invalid {
		_, err := NewDeviceTokens(raw)
		if !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("raw=%q err=%v", raw, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("configuration error leaked a token")
		}
	}
}
