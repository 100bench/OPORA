package scenarios

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/100bench/OPORA/internal/app"
	"github.com/100bench/OPORA/internal/domain"
	deviceauth "github.com/100bench/OPORA/internal/infrastructure/auth"
	"github.com/100bench/OPORA/internal/service"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time                     { return c.now }
func (fixedClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

type plannerFunc func(context.Context, domain.PlanInput) (domain.PlanOutput, error)

func (f plannerFunc) Plan(ctx context.Context, in domain.PlanInput) (domain.PlanOutput, error) {
	return f(ctx, in)
}

type transcriberStub struct{}

func (transcriberStub) Transcribe(context.Context, []byte) (string, error) { return "тест", nil }

type apiError struct {
	Error struct {
		Code domain.ErrorCode `json:"code"`
	} `json:"error"`
}

func scenarioHandler(t *testing.T, planner service.Planner) http.Handler {
	t.Helper()
	authenticator, err := deviceauth.NewDeviceTokens(`{"device-a":"token-a"}`)
	if err != nil {
		t.Fatalf("auth config: %v", err)
	}
	svc := service.New(planner, transcriberStub{}, fixedClock{now: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}, service.DefaultConfig())
	return app.NewRouter(app.Dependencies{Assistant: svc, Auth: authenticator})
}

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer token-a")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode status=%d body=%q: %v", w.Code, w.Body.String(), err)
	}
	return out
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code domain.ErrorCode) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	got := decodeJSON[apiError](t, w)
	if got.Error.Code != code {
		t.Fatalf("error=%q want=%q body=%s", got.Error.Code, code, w.Body.String())
	}
}

func createSession(t *testing.T, h http.Handler) domain.Session {
	t.Helper()
	w := postJSON(t, h, "/v1/sessions", map[string]any{
		"apps":     []domain.App{{Ref: "app-maps", Label: "Карты"}},
		"contacts": []domain.Contact{{Ref: "fav-mom", Label: "Мама"}},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	return decodeJSON[domain.Session](t, w)
}

func screen(version string, nodes ...domain.ScreenNode) domain.ScreenSnapshot {
	return domain.ScreenSnapshot{Package: "ru.example.safe", Version: version, Width: 1080, Height: 1920, Nodes: append([]domain.ScreenNode{}, nodes...)}
}

func safeNode(id, text string) domain.ScreenNode {
	return domain.ScreenNode{ID: id, Text: text, Bounds: domain.Bounds{Left: 10, Top: 20, Right: 500, Bottom: 140}, Enabled: true, Visible: true}
}

func turnRequest(id, text string, snapshot domain.ScreenSnapshot, image *domain.ImageInput) domain.TurnRequest {
	return domain.TurnRequest{RequestID: id, Text: text, Screen: snapshot, Image: image}
}

func TestScenario_ProtectedAndSensitiveImagePathsStopBeforeProvider(t *testing.T) {
	var calls atomic.Int32
	h := scenarioHandler(t, plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
		calls.Add(1)
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "provider must not run"}}, nil
	}))
	session := createSession(t, h)

	protected := screen("protected-v1", safeNode("safe-1", "Текст"))
	protected.Protected = true
	assertRefuse(t, postJSON(t, h, "/v1/sessions/"+session.ID+"/turns", turnRequest("protected-1", "объясни экран", protected, nil)))

	sensitiveScreen := screen("sensitive-v1", safeNode("safe-2", "Текст"))
	sensitiveScreen.Sensitive = true
	assertRefuse(t, postJSON(t, h, "/v1/sessions/"+session.ID+"/turns", turnRequest("sensitive-screen-1", "объясни экран", sensitiveScreen, nil)))

	sensitiveNode := safeNode("secret-1", "Секрет")
	sensitiveNode.Sensitive = true
	imageSnapshot := screen("sensitive-image-v1", sensitiveNode)
	imageInput := &domain.ImageInput{MediaType: "image/png", DataBase64: validPNGBase64(t), Consent: true, ScreenVersion: "sensitive-image-v1"}
	refused := postJSON(t, h, "/v1/sessions/"+session.ID+"/turns", turnRequest("sensitive-image-1", "объясни изображение", imageSnapshot, imageInput))
	assertRefuse(t, refused)
	if calls.Load() != 0 {
		t.Fatalf("provider called %d times for protected/sensitive input", calls.Load())
	}
}

func TestScenario_TreeOnlySanitizesBeforeGroundedExplanation(t *testing.T) {
	var mu sync.Mutex
	var captured domain.PlanInput
	h := scenarioHandler(t, plannerFunc(func(_ context.Context, in domain.PlanInput) (domain.PlanOutput, error) {
		mu.Lock()
		captured = in
		mu.Unlock()
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "На экране показан безопасный заголовок."}}, nil
	}))
	session := createSession(t, h)
	safe := safeNode("safe-title", "Безопасный заголовок")
	secret := safeNode("secret-field", "1234")
	secret.Sensitive = true

	w := postJSON(t, h, "/v1/sessions/"+session.ID+"/turns", turnRequest("tree-explain-1", "объясни экран", screen("tree-v1", safe, secret), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("turn status=%d body=%s", w.Code, w.Body.String())
	}
	reply := decodeJSON[domain.Reply](t, w)
	if reply.Kind != domain.ReplyExplain || reply.Text == "" || reply.Action != nil || reply.ScreenVersion != "tree-v1" || reply.Generation == 0 {
		t.Fatalf("bad grounded explanation: %#v", reply)
	}
	mu.Lock()
	defer mu.Unlock()
	if captured.Image != nil || len(captured.Screen.Nodes) != 1 || captured.Screen.Nodes[0].ID != "safe-title" || captured.Screen.Nodes[0].Sensitive {
		t.Fatalf("provider received unsanitized semantic context: %#v", captured)
	}
}

func TestScenario_ConsentedImageExplanationIsVersionBound(t *testing.T) {
	var calls atomic.Int32
	var validProviderInput atomic.Bool
	h := scenarioHandler(t, plannerFunc(func(_ context.Context, in domain.PlanInput) (domain.PlanOutput, error) {
		calls.Add(1)
		validProviderInput.Store(in.Image != nil && in.Image.Consent && in.Image.MediaType == "image/png" && in.Image.ScreenVersion == in.Screen.Version && len(in.Screen.Nodes) == 0)
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "На изображении безопасный тестовый экран."}}, nil
	}))
	session := createSession(t, h)
	snapshot := screen("image-v1")
	imageInput := &domain.ImageInput{MediaType: "image/png", DataBase64: validPNGBase64(t), Consent: true, ScreenVersion: "image-v1"}
	w := postJSON(t, h, "/v1/sessions/"+session.ID+"/turns", turnRequest("image-explain-1", "объясни изображение", snapshot, imageInput))
	if w.Code != http.StatusOK {
		t.Fatalf("image turn status=%d body=%s", w.Code, w.Body.String())
	}
	reply := decodeJSON[domain.Reply](t, w)
	if reply.Kind != domain.ReplyExplain || reply.Action != nil || reply.ScreenVersion != "image-v1" || calls.Load() != 1 || !validProviderInput.Load() {
		t.Fatalf("bad image explanation reply=%#v calls=%d", reply, calls.Load())
	}

	stale := *imageInput
	stale.ScreenVersion = "other-version"
	assertRefuse(t, postJSON(t, h, "/v1/sessions/"+session.ID+"/turns", turnRequest("image-stale-1", "объясни изображение", snapshot, &stale)))
	withoutConsent := *imageInput
	withoutConsent.Consent = false
	assertRefuse(t, postJSON(t, h, "/v1/sessions/"+session.ID+"/turns", turnRequest("image-no-consent-1", "объясни изображение", snapshot, &withoutConsent)))
	if calls.Load() != 1 {
		t.Fatalf("stale image reached provider; calls=%d", calls.Load())
	}
}

func assertRefuse(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("refuse status=%d body=%s", w.Code, w.Body.String())
	}
	reply := decodeJSON[domain.Reply](t, w)
	if reply.Kind != domain.ReplyRefuse || reply.Action != nil {
		t.Fatalf("did not fail closed: %#v", reply)
	}
}

func TestScenario_ModelCannotInventActionForExplanation(t *testing.T) {
	var calls atomic.Int32
	h := scenarioHandler(t, plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
		calls.Add(1)
		return domain.PlanOutput{Reply: domain.Reply{
			Kind: domain.ReplyActionProposal, Text: "Открыть приложение", Action: &domain.ActionProposal{
				Kind: domain.ActionOpenApp, AppRef: "app-maps", AppLabel: "Карты", DisplayText: "Карты",
			},
		}}, nil
	}))
	session := createSession(t, h)
	w := postJSON(t, h, "/v1/sessions/"+session.ID+"/turns", turnRequest("malicious-model-1", "объясни экран", screen("malicious-v1", safeNode("safe-title", "Заголовок")), nil))
	assertError(t, w, http.StatusUnprocessableEntity, domain.CodeUnsafe)
	if calls.Load() != 1 {
		t.Fatalf("planner calls=%d want=1", calls.Load())
	}
}

func validPNGBase64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}
