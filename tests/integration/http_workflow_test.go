package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

type transcriberFunc func(context.Context, []byte) (string, error)

func (f transcriberFunc) Transcribe(ctx context.Context, wav []byte) (string, error) {
	return f(ctx, wav)
}

type apiError struct {
	Error struct {
		Code domain.ErrorCode `json:"code"`
	} `json:"error"`
}

func newHandler(t *testing.T, planner service.Planner, transcriber service.Transcriber) http.Handler {
	t.Helper()
	if planner == nil {
		planner = plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
			return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "Экран объяснён."}}, nil
		})
	}
	if transcriber == nil {
		transcriber = transcriberFunc(func(context.Context, []byte) (string, error) { return "открой Карты", nil })
	}
	authenticator, err := deviceauth.NewDeviceTokens(`{"device-a":"token-a","device-b":"token-b"}`)
	if err != nil {
		t.Fatalf("auth config: %v", err)
	}
	svc := service.New(planner, transcriber, fixedClock{now: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}, service.DefaultConfig())
	return app.NewRouter(app.Dependencies{Assistant: svc, Auth: authenticator})
}

func postJSON(t *testing.T, h http.Handler, token, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
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
		t.Fatalf("error code=%q want=%q body=%s", got.Error.Code, code, w.Body.String())
	}
}

func createSession(t *testing.T, h http.Handler, token string) domain.Session {
	t.Helper()
	w := postJSON(t, h, token, "/v1/sessions", map[string]any{
		"apps":     []domain.App{{Ref: "app-maps", Label: "Карты", Aliases: []string{"карты"}}},
		"contacts": []domain.Contact{{Ref: "fav-mom", Label: "Мама", Aliases: []string{"маме"}}},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	return decodeJSON[domain.Session](t, w)
}

func validScreen(version string) domain.ScreenSnapshot {
	return domain.ScreenSnapshot{
		Package: "ru.example.safe", Version: version, Width: 1080, Height: 1920,
		Nodes: []domain.ScreenNode{{
			ID: "node-title", Text: "Главный экран", Bounds: domain.Bounds{Left: 0, Top: 0, Right: 500, Bottom: 120}, Enabled: true, Visible: true,
		}},
	}
}

func turn(t *testing.T, h http.Handler, token, sessionID, requestID, text, version string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, h, token, "/v1/sessions/"+sessionID+"/turns", domain.TurnRequest{
		RequestID: requestID, Text: text, Screen: validScreen(version),
	})
}

func TestHTTPWorkflow_OpenAppConfirmResultAndIdempotency(t *testing.T) {
	var providerCalls atomic.Int32
	h := newHandler(t, plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
		providerCalls.Add(1)
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "unexpected provider call"}}, nil
	}), nil)

	unauthorized := postJSON(t, h, "", "/v1/sessions", map[string]any{"apps": []domain.App{}})
	assertError(t, unauthorized, http.StatusUnauthorized, domain.CodeUnauthorized)

	session := createSession(t, h, "token-a")
	w := turn(t, h, "token-a", session.ID, "turn-open-1", "открой Карты", "screen-1")
	if w.Code != http.StatusOK {
		t.Fatalf("turn status=%d body=%s", w.Code, w.Body.String())
	}
	reply := decodeJSON[domain.Reply](t, w)
	if reply.Kind != domain.ReplyActionProposal || reply.Action == nil || reply.Action.Kind != domain.ActionOpenApp || reply.Action.AppRef != "app-maps" || reply.Action.ActionID == "" {
		t.Fatalf("un-grounded OPEN_APP proposal: %#v", reply)
	}
	if reply.Generation == 0 || reply.ScreenVersion != "screen-1" {
		t.Fatalf("server binding missing: %#v", reply)
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("explicit deterministic action called planner %d times", providerCalls.Load())
	}

	confirm := domain.ConfirmationRequest{RequestID: "confirm-open-1", ActionID: reply.Action.ActionID, ScreenVersion: reply.ScreenVersion, Generation: reply.Generation, Decision: domain.DecisionConfirm}
	first := postJSON(t, h, "token-a", "/v1/sessions/"+session.ID+"/confirmations", confirm)
	if first.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", first.Code, first.Body.String())
	}
	grant := decodeJSON[domain.ConfirmationResult](t, first)
	if grant.Status != "confirmed" || grant.Grant == nil || grant.Grant.ActionID != reply.Action.ActionID || grant.Grant.Kind != domain.ActionOpenApp || grant.Grant.AppRef != "app-maps" {
		t.Fatalf("grant does not preserve proposal: %#v", grant)
	}
	replay := postJSON(t, h, "token-a", "/v1/sessions/"+session.ID+"/confirmations", confirm)
	if replay.Code != http.StatusOK {
		t.Fatalf("confirm replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	replayedGrant := decodeJSON[domain.ConfirmationResult](t, replay)
	if replayedGrant.Grant == nil || replayedGrant.Grant.ActionID != grant.Grant.ActionID || !replayedGrant.Grant.ExpiresAt.Equal(grant.Grant.ExpiresAt) {
		t.Fatalf("replay minted a different grant: first=%#v replay=%#v", grant, replayedGrant)
	}
	altered := confirm
	altered.Decision = domain.DecisionDecline
	assertError(t, postJSON(t, h, "token-a", "/v1/sessions/"+session.ID+"/confirmations", altered), http.StatusConflict, domain.CodeConflict)

	result := domain.ActionResultRequest{RequestID: "result-open-1", ActionID: grant.Grant.ActionID, Status: "opened"}
	for i := 0; i < 2; i++ {
		got := postJSON(t, h, "token-a", "/v1/sessions/"+session.ID+"/results", result)
		if got.Code != http.StatusOK {
			t.Fatalf("result attempt %d status=%d body=%s", i+1, got.Code, got.Body.String())
		}
	}
}

func TestHTTPWorkflow_DialerDeclineIsolationAndTerminalCancel(t *testing.T) {
	h := newHandler(t, nil, nil)

	declinedSession := createSession(t, h, "token-a")
	proposalResponse := turn(t, h, "token-a", declinedSession.ID, "turn-dial-decline", "позвони маме", "screen-dial-1")
	if proposalResponse.Code != http.StatusOK {
		t.Fatalf("dial turn status=%d body=%s", proposalResponse.Code, proposalResponse.Body.String())
	}
	proposal := decodeJSON[domain.Reply](t, proposalResponse)
	if proposal.Action == nil || proposal.Action.Kind != domain.ActionPrepareDialer || proposal.Action.ContactRef != "fav-mom" || proposal.Action.ActionID == "" {
		t.Fatalf("bad dialer proposal: %#v", proposal)
	}
	foreignConfirm := domain.ConfirmationRequest{RequestID: "foreign-confirm", ActionID: proposal.Action.ActionID, ScreenVersion: proposal.ScreenVersion, Generation: proposal.Generation, Decision: domain.DecisionConfirm}
	assertError(t, postJSON(t, h, "token-b", "/v1/sessions/"+declinedSession.ID+"/confirmations", foreignConfirm), http.StatusNotFound, domain.CodeNotFound)

	decline := foreignConfirm
	decline.RequestID = "decline-dial-1"
	decline.Decision = domain.DecisionDecline
	declined := postJSON(t, h, "token-a", "/v1/sessions/"+declinedSession.ID+"/confirmations", decline)
	if declined.Code != http.StatusOK {
		t.Fatalf("decline status=%d body=%s", declined.Code, declined.Body.String())
	}
	declineResult := decodeJSON[domain.ConfirmationResult](t, declined)
	if declineResult.Status != "declined" || declineResult.Grant != nil {
		t.Fatalf("decline created grant: %#v", declineResult)
	}
	replayedDecline := postJSON(t, h, "token-a", "/v1/sessions/"+declinedSession.ID+"/confirmations", decline)
	if replayedDecline.Code != http.StatusOK || decodeJSON[domain.ConfirmationResult](t, replayedDecline).Grant != nil {
		t.Fatalf("decline replay changed outcome: status=%d body=%s", replayedDecline.Code, replayedDecline.Body.String())
	}
	secondDecision := decline
	secondDecision.RequestID = "confirm-after-decline"
	secondDecision.Decision = domain.DecisionConfirm
	assertError(t, postJSON(t, h, "token-a", "/v1/sessions/"+declinedSession.ID+"/confirmations", secondDecision), http.StatusConflict, domain.CodeConflict)

	issuedSession := createSession(t, h, "token-a")
	issuedReply := decodeJSON[domain.Reply](t, turn(t, h, "token-a", issuedSession.ID, "turn-dial-issued", "позвони маме", "screen-dial-2"))
	confirm := domain.ConfirmationRequest{RequestID: "confirm-dial-2", ActionID: issuedReply.Action.ActionID, ScreenVersion: issuedReply.ScreenVersion, Generation: issuedReply.Generation, Decision: domain.DecisionConfirm}
	confirmed := postJSON(t, h, "token-a", "/v1/sessions/"+issuedSession.ID+"/confirmations", confirm)
	if confirmed.Code != http.StatusOK {
		t.Fatalf("dial confirm status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	issued := decodeJSON[domain.ConfirmationResult](t, confirmed)
	if issued.Grant == nil || issued.Grant.Kind != domain.ActionPrepareDialer || issued.Grant.ContactRef != "fav-mom" {
		t.Fatalf("bad dial grant: %#v", issued)
	}
	result := domain.ActionResultRequest{RequestID: "result-dial-2", ActionID: issued.Grant.ActionID, Status: "dialer_presented"}
	assertError(t, postJSON(t, h, "token-b", "/v1/sessions/"+issuedSession.ID+"/results", result), http.StatusNotFound, domain.CodeNotFound)
	if owner := postJSON(t, h, "token-a", "/v1/sessions/"+issuedSession.ID+"/results", result); owner.Code != http.StatusOK {
		t.Fatalf("owner result status=%d body=%s", owner.Code, owner.Body.String())
	}

	cancelSession := createSession(t, h, "token-a")
	assertError(t, postJSON(t, h, "token-b", "/v1/sessions/"+cancelSession.ID+"/cancel", domain.CancelRequest{RequestID: "foreign-cancel"}), http.StatusNotFound, domain.CodeNotFound)
	if cancelled := postJSON(t, h, "token-a", "/v1/sessions/"+cancelSession.ID+"/cancel", domain.CancelRequest{RequestID: "owner-cancel"}); cancelled.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	assertError(t, turn(t, h, "token-a", cancelSession.ID, "turn-after-cancel", "что на экране", "screen-after-cancel"), http.StatusConflict, domain.CodeConflict)
}

func TestHTTPWorkflow_ValidWAVTranscriptionFeedsTurn(t *testing.T) {
	var transcriberCalls atomic.Int32
	var receivedValidWAV atomic.Bool
	h := newHandler(t, nil, transcriberFunc(func(_ context.Context, wav []byte) (string, error) {
		transcriberCalls.Add(1)
		receivedValidWAV.Store(len(wav) >= 44 && string(wav[:4]) == "RIFF")
		return "открой Карты", nil
	}))
	session := createSession(t, h, "token-a")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("request_id", "audio-1"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("audio", "voice.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(validWAV()); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/transcriptions", &body)
	req.Header.Set("Authorization", "Bearer token-a")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("transcribe status=%d body=%s", w.Code, w.Body.String())
	}
	transcription := decodeJSON[domain.Transcription](t, w)
	if transcription.Text != "открой Карты" || transcriberCalls.Load() != 1 || !receivedValidWAV.Load() {
		t.Fatalf("transcription=%#v calls=%d", transcription, transcriberCalls.Load())
	}

	turnResponse := turn(t, h, "token-a", session.ID, "turn-from-audio-1", transcription.Text, "screen-audio-1")
	if turnResponse.Code != http.StatusOK {
		t.Fatalf("turn status=%d body=%s", turnResponse.Code, turnResponse.Body.String())
	}
	reply := decodeJSON[domain.Reply](t, turnResponse)
	if reply.Action == nil || reply.Action.Kind != domain.ActionOpenApp || reply.Action.AppRef != "app-maps" {
		t.Fatalf("audio-derived action not grounded: %#v", reply)
	}
}

func validWAV() []byte {
	const samples = 1600
	dataBytes := samples * 2
	b := make([]byte, 44+dataBytes)
	copy(b[0:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], uint32(36+dataBytes))
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	binary.LittleEndian.PutUint16(b[20:22], 1)
	binary.LittleEndian.PutUint16(b[22:24], 1)
	binary.LittleEndian.PutUint32(b[24:28], 16000)
	binary.LittleEndian.PutUint32(b[28:32], 32000)
	binary.LittleEndian.PutUint16(b[32:34], 2)
	binary.LittleEndian.PutUint16(b[34:36], 16)
	copy(b[36:40], "data")
	binary.LittleEndian.PutUint32(b[40:44], uint32(dataBytes))
	return b
}
