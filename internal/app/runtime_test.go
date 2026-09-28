package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRuntimeMissingProviderKeyIsAliveButNotReady(t *testing.T) {
	env := map[string]string{
		"LISTEN_ADDR":        "127.0.0.1:8080",
		"DEVICE_TOKENS_JSON": `{"device":"token"}`,
	}
	runtime, err := NewRuntime(func(key string) string { return env[key] }, nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/health": http.StatusOK, "/ready": http.StatusServiceUnavailable} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		runtime.Handler().ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	server := runtime.Server()
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 || server.MaxHeaderBytes <= 0 {
		t.Fatalf("unsafe server timeouts: %#v", server)
	}
	if server.WriteTimeout <= 20*time.Second {
		t.Fatalf("write timeout must allow the bounded provider call: %s", server.WriteTimeout)
	}
}

func TestRuntimeRejectsInvalidAuthConfiguration(t *testing.T) {
	_, err := NewRuntime(func(key string) string {
		if key == "DEVICE_TOKENS_JSON" {
			return `{"device":"duplicate","other":"duplicate"}`
		}
		return ""
	}, nil)
	if err == nil {
		t.Fatal("duplicate bearer token accepted")
	}
}
