package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/100bench/OPORA/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestNFR20_ResponsesWireContractAndServerOwnedFields(t *testing.T) {
	var got map[string]any
	var method, path, auth, ctype string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		method, path, auth, ctype = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		return response(200, `{"status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"kind\":\"explain\",\"text\":\"ok\",\"highlight_node_ids\":[],\"action\":null}"}]}]}`), nil
	})}
	out, err := New("https://provider.invalid", "secret", hc).Plan(context.Background(), domain.PlanInput{Text: "что это", Screen: domain.ScreenSnapshot{Version: "v"}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if method != "POST" || path != "/responses" || auth != "Bearer secret" || ctype != "application/json" {
		t.Fatalf("wire=%q %q %q %q", method, path, auth, ctype)
	}
	if got["model"] != PlannerModel || got["store"] != false {
		t.Fatalf("model/store=%#v", got)
	}
	reasoning := asMap(t, got["reasoning"])
	if reasoning["effort"] != "none" {
		t.Fatalf("reasoning=%#v", reasoning)
	}
	format := asMap(t, asMap(t, got["text"])["format"])
	if format["type"] != "json_schema" || format["strict"] != true || format["name"] != "opora_reply" {
		t.Fatalf("format=%#v", format)
	}
	assertStrictObjectSchemas(t, asMap(t, format["schema"]))
	if out.Reply.Generation != 0 || out.Reply.ScreenVersion != "" || (out.Reply.Action != nil && out.Reply.Action.ActionID != "") {
		t.Fatalf("provider set server-owned fields: %#v", out.Reply)
	}
}

func TestNFR21_ResponsesMalformedAmbiguousAndBounded(t *testing.T) {
	bodies := []string{`{}`, `{"status":"incomplete","output":[]}`, `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"},{"type":"output_text","text":"{}"}]}]}`, `{"status":"completed","output":[{"type":"function_call","name":"x"}]}`, `{"status":"completed","output":[{"type":"unknown"}]}`, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"unknown\":1}"}]}]}`, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"kind\":\"explain\",\"text\":\"x\",\"highlight_node_ids\":[],\"action\":null,\"generation\":9}"}]}]}`, `not json`, strings.Repeat("x", (1<<20)+1)}
	for i, body := range bodies {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })}
			if _, err := New("https://provider.invalid", "secret", hc).Plan(context.Background(), domain.PlanInput{}); err == nil {
				t.Fatal("unsafe provider output accepted")
			}
		})
	}
}

func TestNFR22_TranscriptionWireContract(t *testing.T) {
	var model, auth, ctype, method, path string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		method, path, auth, ctype = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		// #nosec G120 -- the production adapter constructs this bounded test body.
		_ = r.ParseMultipartForm(2 << 20)
		model = r.FormValue("model")
		return response(200, `{"text":"привет"}`), nil
	})}
	text, err := New("https://provider.invalid", "secret", hc).Transcribe(context.Background(), []byte("RIFF-WAVE"))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "привет" || model != TranscriptionModel || method != "POST" || path != "/audio/transcriptions" || auth != "Bearer secret" || !strings.HasPrefix(ctype, "multipart/form-data;") {
		t.Fatalf("wire=%q/%q/%q/%q/%q/%q", text, model, method, path, auth, ctype)
	}
}

func TestNFR23_ProviderNon2xxInFlightCancelAndSecretFreeErrors(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(429, "upstream-secret-body"), nil })}
	_, err := New("https://provider.invalid", "api-key-secret", hc).Plan(context.Background(), domain.PlanInput{Text: "user-secret"})
	if err == nil {
		t.Fatal("non-2xx accepted")
	}
	for _, secret := range []string{"api-key-secret", "upstream-secret-body", "user-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("sensitive data leaked: %v", err)
		}
	}
	entered := make(chan struct{})
	blocking := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, e := New("https://provider.invalid", "secret", blocking).Plan(ctx, domain.PlanInput{})
		done <- e
	}()
	select {
	case err := <-done:
		t.Fatalf("transport not invoked: %v", err)
	case <-entered:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("want object, got %T (%#v)", v, v)
	}
	return m
}
func assertStrictObjectSchemas(t *testing.T, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "object" {
			if x["additionalProperties"] != false {
				t.Fatalf("object allows extra fields: %#v", x)
			}
			props := asMap(t, x["properties"])
			required, ok := x["required"].([]any)
			if !ok || len(required) != len(props) {
				t.Fatalf("required/properties mismatch: %#v", x)
			}
		}
		for _, child := range x {
			assertStrictObjectSchemas(t, child)
		}
	case []any:
		for _, child := range x {
			assertStrictObjectSchemas(t, child)
		}
	}
}
