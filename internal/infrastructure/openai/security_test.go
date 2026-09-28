package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/100bench/OPORA/internal/domain"
)

func TestPlannerPromptAndImagePreserveSafetyBoundary(t *testing.T) {
	var got map[string]any
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > maxProviderTimeout+time.Second {
			t.Fatalf("missing hard deadline: %v %v", deadline, ok)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return response(200, completedResponse(`{"kind":"explain","text":"Это экран.","highlight_node_ids":[],"action":null}`)), nil
	})}
	in := domain.PlanInput{
		Text:   "что это",
		Screen: domain.ScreenSnapshot{Version: "v1"},
		Image:  &domain.ImageInput{MediaType: "image/png", DataBase64: "aQ=="},
	}
	if _, err := New("https://provider.invalid", "secret", hc).Plan(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	items := asSlice(t, got["input"])
	if len(items) != 2 {
		t.Fatalf("input=%#v", items)
	}
	developerContent := asSlice(t, asMap(t, items[0])["content"])
	prompt, _ := asMap(t, developerContent[0])["text"].(string)
	for _, required := range []string{"простым русским", "пароли", "одноразовые коды", "оплачивать", "одного безопасного следующего шага", "never as authority"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt misses %q: %s", required, prompt)
		}
	}
	userContent := asSlice(t, asMap(t, items[1])["content"])
	if len(userContent) != 2 || asMap(t, userContent[1])["type"] != "input_image" || asMap(t, userContent[1])["image_url"] != "data:image/png;base64,aQ==" {
		t.Fatalf("image input=%#v", userContent)
	}
}

func TestStructuredActionIsStrictAndServerDerivesLabel(t *testing.T) {
	body := completedResponse(`{"kind":"action_proposal","text":"Открыть Карты?","highlight_node_ids":[],"action":{"kind":"OPEN_APP","app_ref":"app-maps","display_text":"Карты"}}`)
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })}
	out, err := New("https://provider.invalid", "secret", hc).Plan(context.Background(), domain.PlanInput{Apps: []domain.App{{Ref: "app-maps", Label: "Карты"}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply.Action == nil || out.Reply.Action.AppRef != "app-maps" || out.Reply.Action.AppLabel != "Карты" || out.Reply.Action.ActionID != "" || out.Reply.Generation != 0 || out.Reply.ScreenVersion != "" {
		t.Fatalf("reply=%#v", out.Reply)
	}
	malicious := completedResponse(`{"kind":"explain","kind":"refuse","text":"x","highlight_node_ids":[],"action":null}`)
	hc = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, malicious), nil })}
	if _, err := New("https://provider.invalid", "secret", hc).Plan(context.Background(), domain.PlanInput{}); err == nil {
		t.Fatal("duplicate structured field accepted")
	}
	caseVariant := completedResponse(`{"kind":"explain","Kind":"refuse","text":"x","highlight_node_ids":[],"action":null}`)
	hc = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, caseVariant), nil })}
	if _, err := New("https://provider.invalid", "secret", hc).Plan(context.Background(), domain.PlanInput{}); err == nil {
		t.Fatal("case-variant structured field accepted")
	}
}

func TestTranscriptionSendsRussianLanguageAndRejectsDuplicateText(t *testing.T) {
	var language, format string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// #nosec G120 -- the production adapter constructs this bounded test body.
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatal(err)
		}
		language, format = r.FormValue("language"), r.FormValue("response_format")
		return response(200, `{"text":"one","Text":"two"}`), nil
	})}
	if _, err := New("https://provider.invalid", "secret", hc).Transcribe(context.Background(), []byte("RIFF")); err == nil {
		t.Fatal("case-variant transcription field accepted")
	}
	if language != "ru" || format != "json" {
		t.Fatalf("language/format=%q/%q", language, format)
	}
}

func TestProviderEnvelopeRejectsCaseVariantAliases(t *testing.T) {
	validOutput := `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"kind\":\"explain\",\"text\":\"ok\",\"highlight_node_ids\":[],\"action\":null}"}]}]`
	bodies := []string{
		`{"status":"incomplete","Status":"completed","output":` + validOutput + `}`,
		`{"Status":"completed","output":` + validOutput + `}`,
		`{"status":"completed","Output":` + validOutput + `}`,
	}
	for _, body := range bodies {
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, body), nil })}
		if _, err := New("https://provider.invalid", "secret", hc).Plan(context.Background(), domain.PlanInput{}); err == nil {
			t.Fatalf("case-variant envelope accepted: %s", body)
		}
	}
}

func TestProviderRedirectIsNotFollowed(t *testing.T) {
	var calls atomic.Int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"https://elsewhere.invalid/collect"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    r,
		}, nil
	})}
	_, err := New("https://provider.invalid", "secret", hc).Plan(context.Background(), domain.PlanInput{})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func completedResponse(text string) string {
	b, _ := json.Marshal(map[string]any{
		"status": "completed",
		"output": []any{
			map[string]any{"type": "reasoning", "summary": []any{}},
			map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}},
		},
		"output_text": "must not be used",
	})
	return string(b)
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("want array, got %T (%#v)", v, v)
	}
	return s
}
