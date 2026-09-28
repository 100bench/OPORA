package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/100bench/OPORA/internal/domain"
)

type authStub struct{}

func (authStub) Authenticate(token string) (domain.Device, error) {
	if token != "good" {
		return domain.Device{}, errors.New("bad token")
	}
	return domain.Device{ID: "device-1"}, nil
}

type assistantStub struct {
	err       error
	called    string
	device    domain.Device
	sessionID string
	config    domain.SessionConfig
}

func (s *assistantStub) CreateSession(_ context.Context, c domain.SessionConfig) (domain.Session, error) {
	s.called = "sessions"
	s.config = c
	return domain.Session{ID: "s1", ExpiresAt: time.Unix(1, 0)}, s.err
}
func (s *assistantStub) Transcribe(_ context.Context, d domain.Device, _ domain.TranscriptionRequest) (domain.Transcription, error) {
	s.called = "transcriptions"
	s.device = d
	return domain.Transcription{Text: "текст"}, s.err
}
func (s *assistantStub) Turn(_ context.Context, d domain.Device, sid string, _ domain.TurnRequest) (domain.Reply, error) {
	s.called = "turns"
	s.device = d
	s.sessionID = sid
	return domain.Reply{Kind: domain.ReplyExplain, Text: "ok"}, s.err
}
func (s *assistantStub) Confirm(_ context.Context, d domain.Device, sid string, _ domain.ConfirmationRequest) (domain.ConfirmationResult, error) {
	s.called = "confirmations"
	s.device = d
	s.sessionID = sid
	return domain.ConfirmationResult{Status: "confirmed", Grant: &domain.ActionGrant{ActionID: "a1"}}, s.err
}
func (s *assistantStub) ReportResult(_ context.Context, d domain.Device, sid string, _ domain.ActionResultRequest) error {
	s.called = "results"
	s.device = d
	s.sessionID = sid
	return s.err
}
func (s *assistantStub) Cancel(_ context.Context, d domain.Device, sid string, _ domain.CancelRequest) error {
	s.called = "cancel"
	s.device = d
	s.sessionID = sid
	return s.err
}
func (s *assistantStub) Ready(context.Context) error { s.called = "ready"; return s.err }

func request(t *testing.T, h http.Handler, method, path, body, contentType string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if auth {
		r.Header.Set("Authorization", "Bearer good")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHTTP_FR01_AllEndpointsAndAuth(t *testing.T) {
	cases := []struct {
		name, method, path, body string
		want                     int
		call                     string
	}{
		{"health", "GET", "/health", "", 200, ""}, {"ready", "GET", "/ready", "", 200, "ready"},
		{"sessions", "POST", "/v1/sessions", `{"apps":[{"ref":"maps","label":"Карты"}]}`, 201, "sessions"},
		{"turns", "POST", "/v1/sessions/s1/turns", `{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":[]}}`, 200, "turns"},
		{"confirmations", "POST", "/v1/sessions/s1/confirmations", `{"request_id":"r","action_id":"a","screen_version":"v","generation":1,"decision":"CONFIRM"}`, 200, "confirmations"},
		{"results", "POST", "/v1/sessions/s1/results", `{"request_id":"r","action_id":"a","status":"dialer_presented"}`, 200, "results"},
		{"cancel", "POST", "/v1/sessions/s1/cancel", `{"request_id":"r"}`, 200, "cancel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &assistantStub{}
			w := request(t, NewRouter(Dependencies{Assistant: s, Auth: authStub{}}), tc.method, tc.path, tc.body, "application/json", true)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.call != "" && s.called != tc.call {
				t.Fatalf("called=%q", s.called)
			}
			if strings.HasPrefix(tc.path, "/v1") && s.device.ID != "device-1" && tc.name != "sessions" {
				t.Fatalf("device=%q", s.device.ID)
			}
			if strings.Contains(tc.path, "/sessions/s1/") && s.sessionID != "s1" {
				t.Fatalf("session=%q", s.sessionID)
			}
			if tc.name == "sessions" && s.config.DeviceID != "device-1" {
				t.Fatalf("session device=%q", s.config.DeviceID)
			}
		})
	}
	protected := cases[2:]
	for _, tc := range protected {
		t.Run("auth_"+tc.name, func(t *testing.T) {
			w := request(t, NewRouter(Dependencies{Assistant: &assistantStub{}, Auth: authStub{}}), tc.method, tc.path, tc.body, "application/json", false)
			if w.Code != 401 {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
}

func TestHTTP_FR02_TranscriptionMultipart(t *testing.T) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("request_id", "r1")
	p, _ := mw.CreateFormFile("audio", "voice.wav")
	_, _ = p.Write([]byte("RIFFtest"))
	_ = mw.Close()
	r := httptest.NewRequestWithContext(context.Background(), "POST", "/v1/transcriptions", &b)
	r.Header.Set("Authorization", "Bearer good")
	r.Header.Set("Content-Type", mw.FormDataContentType())
	s := &assistantStub{}
	w := httptest.NewRecorder()
	NewRouter(Dependencies{Assistant: s, Auth: authStub{}}).ServeHTTP(w, r)
	if w.Code != 200 || s.called != "transcriptions" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestHTTP_NFR01_StrictJSONBodyLimitsAndStableErrors(t *testing.T) {
	h := NewRouter(Dependencies{Assistant: &assistantStub{}, Auth: authStub{}})
	for name, body := range map[string]string{"unknown": `{"apps":[],"surprise":1}`, "trailing": `{"apps":[]} {}`, "malformed": `{"apps":`} {
		t.Run(name, func(t *testing.T) {
			w := request(t, h, "POST", "/v1/sessions", body, "application/json", true)
			assertError(t, w, 400, domain.CodeInvalidArgument)
		})
	}
	body := `{"apps":[{"ref":"x","label":"` + strings.Repeat("x", maxJSONBody) + `"}]}`
	w := request(t, h, "POST", "/v1/sessions", body, "application/json", true)
	assertError(t, w, 413, domain.CodePayloadTooLarge)
	w = request(t, h, "POST", "/v1/sessions", `{"apps":[]}`, "text/plain", true)
	assertError(t, w, 415, domain.CodeUnsupportedMedia)
}

func TestHTTP_NFR02_ErrorMappingConsistency(t *testing.T) {
	cases := []struct {
		code   domain.ErrorCode
		status int
	}{{domain.CodeInvalidArgument, 400}, {domain.CodePayloadTooLarge, 413}, {domain.CodeUnsupportedMedia, 415}, {domain.CodeUnauthorized, 401}, {domain.CodeNotFound, 404}, {domain.CodeConflict, 409}, {domain.CodeInProgress, 409}, {domain.CodeExpired, 410}, {domain.CodeUnsafe, 422}, {domain.CodeResourceExhausted, 429}, {domain.CodeProviderUnavailable, 503}, {domain.CodeInternal, 500}}
	for _, tc := range cases {
		s := &assistantStub{err: &domain.AppError{Code: tc.code, Message: "safe"}}
		w := request(t, NewRouter(Dependencies{Assistant: s, Auth: authStub{}}), "POST", "/v1/sessions/s/turns", `{"request_id":"r","text":"x","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":[]}}`, "application/json", true)
		assertError(t, w, tc.status, tc.code)
	}
}

func TestHTTP_NFR03_NoRawInputInLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	secret := "OTP-1234-private"
	s := &assistantStub{err: errors.New("upstream")}
	w := request(t, NewRouter(Dependencies{Assistant: s, Auth: authStub{}, Logger: logger}), "POST", "/v1/sessions/s/turns", `{"request_id":"r","text":"`+secret+`","screen":{"package":"p","version":"v","width":1,"height":1,"nodes":[]}}`, "application/json", true)
	if w.Code != 500 {
		t.Fatalf("status=%d", w.Code)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("raw user input leaked to logs")
	}
	if !strings.Contains(logs.String(), `"msg":"http_request"`) || !strings.Contains(logs.String(), `"status":500`) {
		t.Fatalf("missing safe structured record: %s", logs.String())
	}
}

func TestHTTP_NFR04_RejectDuplicateAudioParts(t *testing.T) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("request_id", "r")
	for i := 0; i < 2; i++ {
		p, _ := mw.CreateFormFile("audio", "v.wav")
		_, _ = io.WriteString(p, "RIFF")
	}
	_ = mw.Close()
	r := httptest.NewRequestWithContext(context.Background(), "POST", "/v1/transcriptions", &b)
	r.Header.Set("Authorization", "Bearer good")
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	NewRouter(Dependencies{Assistant: &assistantStub{}, Auth: authStub{}}).ServeHTTP(w, r)
	assertError(t, w, 400, domain.CodeInvalidArgument)
}

func TestHTTP_NFR05_MultipartTypePartsAndAudioBoundary(t *testing.T) {
	h := NewRouter(Dependencies{Assistant: &assistantStub{}, Auth: authStub{}})
	plain := request(t, h, "POST", "/v1/transcriptions", "x", "text/plain", true)
	assertError(t, plain, 415, domain.CodeUnsupportedMedia)
	makeReq := func(size int, extra bool) *httptest.ResponseRecorder {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		_ = mw.WriteField("request_id", "r")
		if extra {
			_ = mw.WriteField("extra", "x")
		}
		p, _ := mw.CreateFormFile("audio", "v.wav")
		_, _ = p.Write(make([]byte, size))
		_ = mw.Close()
		r := httptest.NewRequestWithContext(context.Background(), "POST", "/v1/transcriptions", &b)
		r.Header.Set("Authorization", "Bearer good")
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := makeReq(1<<20, false); w.Code != 200 {
		t.Fatalf("1MiB boundary status=%d body=%s", w.Code, w.Body.String())
	}
	assertError(t, makeReq((1<<20)+1, false), 400, domain.CodeInvalidArgument)
	assertError(t, makeReq(4, true), 400, domain.CodeInvalidArgument)
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code domain.ErrorCode) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	var e errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != code || e.Error.Message == "" {
		t.Fatalf("bad envelope: %#v", e)
	}
}
