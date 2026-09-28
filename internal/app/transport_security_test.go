package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/100bench/OPORA/internal/domain"
)

func TestJSONRejectsInvalidUTF8DuplicatesAndMissingRequiredFields(t *testing.T) {
	h := NewRouter(Dependencies{Assistant: &assistantStub{}, Auth: authStub{}})
	cases := [][]byte{
		append([]byte(`{"apps":[{"ref":"x","label":"`), append([]byte{0xff}, []byte(`"}]}`)...)...),
		[]byte(`{"apps":[],"apps":[]}`),
		[]byte(`{}`),
		[]byte(`{"apps":null}`),
		[]byte(`{"apps":[{"ref":"x"}]}`),
	}
	for i, body := range cases {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer good")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("case %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
}

func TestTurnJSONEnforcesNestedRequiredFields(t *testing.T) {
	h := NewRouter(Dependencies{Assistant: &assistantStub{}, Auth: authStub{}})
	cases := []string{
		`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1}}`,
		`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":null}}`,
		`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":[{"id":"n","bounds":{"left":0,"top":0,"right":1},"enabled":true,"visible":true}]}}`,
		`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":[{"id":"n","bounds":{"left":0,"top":0,"right":1,"bottom":1},"visible":true}]}}`,
		`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"protected":null,"nodes":[]}}`,
		`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":[{"id":"n","text":null,"bounds":{"left":0,"top":0,"right":1,"bottom":1},"enabled":true,"visible":true}]}}`,
		`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"sensitive":true,"Sensitive":false,"nodes":[]}}`,
		`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":[]},"image":{"media_type":"image/png","data_base64":"aQ==","consent":true,"Consent":false,"screen_version":"v"}}`,
	}
	for i, body := range cases {
		w := request(t, h, http.MethodPost, "/v1/sessions/s/turns", body, "application/json", true)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("case %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
}

func TestErrorAndAccessLogDoNotEchoUntrustedValues(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	secretPath := "secret-session-OTP-9911"
	secretMessage := "provider-body-secret"
	s := &assistantStub{err: &domain.AppError{Code: domain.CodeProviderUnavailable, Message: secretMessage}}
	h := NewRouter(Dependencies{Assistant: s, Auth: authStub{}, Logger: logger})
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions/"+secretPath+"/turns?secret=query", strings.NewReader(`{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":[]}}`))
	r.Header.Set("Authorization", "Bearer good")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Request-ID", "unsafe request id with spaces")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Message != publicErrorMessage(domain.CodeProviderUnavailable) || envelope.Error.RequestID != "" {
		t.Fatalf("unsafe envelope=%#v", envelope)
	}
	for _, secret := range []string{secretPath, "secret=query", secretMessage, "unsafe request id"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("untrusted value leaked: %q", secret)
		}
	}
	if !strings.Contains(logs.String(), `"operation":"/v1/sessions/{session_id}/turns"`) || !strings.Contains(logs.String(), `"error_code":"PROVIDER_UNAVAILABLE"`) {
		t.Fatalf("missing safe log metadata: %s", logs.String())
	}
}

func TestAuthorizationHeaderIsUnambiguous(t *testing.T) {
	h := NewRouter(Dependencies{Assistant: &assistantStub{}, Auth: authStub{}})
	for _, value := range []string{"Bearer  good", "Bearer good extra", "Bearer good,bad", "Basic good"} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions", strings.NewReader(`{"apps":[]}`))
		r.Header.Set("Authorization", value)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("header=%q status=%d", value, w.Code)
		}
	}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions", strings.NewReader(`{"apps":[]}`))
	r.Header.Add("Authorization", "Bearer good")
	r.Header.Add("Authorization", "Bearer bad")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate headers status=%d", w.Code)
	}
}

func TestReadySanitizesArbitraryDependencyError(t *testing.T) {
	h := NewRouter(Dependencies{Readiness: func(_ context.Context) error { return errors.New("api-key-secret") }})
	w := request(t, h, http.MethodGet, "/ready", "", "", false)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "api-key-secret") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestUnknownApplicationErrorCodeFailsClosed(t *testing.T) {
	s := &assistantStub{err: &domain.AppError{Code: domain.ErrorCode("SECRET_CODE"), Message: "secret"}}
	w := request(t, NewRouter(Dependencies{Assistant: s, Auth: authStub{}}), http.MethodPost, "/v1/sessions/s/cancel", `{"request_id":"r"}`, "application/json", true)
	assertError(t, w, http.StatusInternalServerError, domain.CodeInternal)
}
