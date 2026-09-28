package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/100bench/OPORA/internal/domain"
)

func TestServiceAdditionalReadinessConfigAndSessionValidation(t *testing.T) {
	ready, _ := newHarness(nil)
	if err := ready.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ready.Ready(cancelledCtx); err == nil {
		t.Fatal("cancelled readiness accepted")
	}
	if err := New(nil, nil, nil, Config{}).Ready(context.Background()); err == nil {
		t.Fatal("missing dependencies reported ready")
	}

	cfg := boundedConfig(Config{SessionTTL: time.Hour, ConfirmationTTL: time.Minute, MaxSessions: 101, MaxProviderCalls: 5, ProviderTimeout: time.Minute})
	if cfg != DefaultConfig() {
		t.Fatalf("unbounded config: %#v", cfg)
	}
	small := boundedConfig(Config{SessionTTL: time.Minute, ConfirmationTTL: time.Second, MaxSessions: 1, MaxProviderCalls: 1, ProviderTimeout: time.Second})
	if small.MaxSessions != 1 || small.MaxProviderCalls != 1 {
		t.Fatalf("smaller test limits lost: %#v", small)
	}

	s, _ := newHarness(nil)
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if _, err := s.CreateSession(ctx, domain.SessionConfig{DeviceID: "d"}); err == nil {
		t.Fatal("cancelled create accepted")
	}
	invalidConfigs := []domain.SessionConfig{
		{},
		{DeviceID: "d", Apps: make([]domain.App, 129)},
		{DeviceID: "d", Contacts: []domain.Contact{{Ref: "+7 999 12345", Label: "x"}}},
		{DeviceID: "d", Apps: []domain.App{{Ref: "same", Label: "x"}, {Ref: "same", Label: "y"}}},
		{DeviceID: "d", Apps: []domain.App{{Ref: "r", Label: "", Aliases: nil}}},
		{DeviceID: "d", Apps: []domain.App{{Ref: "r", Label: "x", Aliases: make([]string, 9)}}},
		{DeviceID: "d", Contacts: []domain.Contact{{Ref: "opaque", Label: "x", Aliases: []string{""}}}},
		{DeviceID: "d", Contacts: []domain.Contact{{Ref: "opaque", Label: "Мама +7 (999) 123-45-67"}}},
		{DeviceID: "d", Contacts: []domain.Contact{{Ref: "opaque", Label: "Мама", Aliases: []string{"8 999 123 45 67"}}}},
		{DeviceID: "d", Contacts: []domain.Contact{{Ref: "tel:+79991234567", Label: "Мама"}}},
		{DeviceID: "d", Contacts: []domain.Contact{{Ref: "opaque", Label: "sms:+79991234567"}}},
		{DeviceID: "d", Apps: []domain.App{{Ref: "intent://open", Label: "x"}}},
		{DeviceID: "d", Apps: []domain.App{{Ref: "https://example.invalid", Label: "x"}}},
	}
	for i, input := range invalidConfigs {
		if _, err := s.CreateSession(context.Background(), input); err == nil {
			t.Errorf("invalid config %d accepted", i)
		}
	}
	if resemblesPhoneNumber("opaque-ref-123") {
		t.Fatal("opaque ref misclassified as phone")
	}
	if !resemblesPhoneNumber("12345") {
		t.Fatal("phone-like ref not detected")
	}
	if containsPhoneNumber("Дом 42") || uriLikeTarget("app-2026") {
		t.Fatal("benign digit-bearing metadata rejected")
	}
	if _, err := s.CreateSession(context.Background(), domain.SessionConfig{DeviceID: "d", Apps: []domain.App{{Ref: "app-2026", Label: "Версия 2"}}, Contacts: []domain.Contact{{Ref: "fav-42", Label: "Дом 42"}}}); err != nil {
		t.Fatalf("benign opaque metadata: %v", err)
	}
}

func TestServiceAdditionalTranscriptionIdempotencyAndErrors(t *testing.T) {
	var calls atomic.Int32
	c := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := New(
		plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
			return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "ok"}}, nil
		}),
		transcriberFunc(func(context.Context, []byte) (string, error) {
			calls.Add(1)
			return "текст", nil
		}), c, DefaultConfig())
	req := domain.TranscriptionRequest{RequestID: "audio", WAV: validWAV()}
	first, err := s.Transcribe(context.Background(), domain.Device{ID: "d"}, req)
	if err != nil || first.Text != "текст" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	replay, err := s.Transcribe(context.Background(), domain.Device{ID: "d"}, req)
	if err != nil || replay != first || calls.Load() != 1 {
		t.Fatalf("replay=%#v err=%v calls=%d", replay, err, calls.Load())
	}
	changed := req
	changed.WAV = append([]byte(nil), req.WAV...)
	changed.WAV[len(changed.WAV)-1] = 1
	if _, err := s.Transcribe(context.Background(), domain.Device{ID: "d"}, changed); !isCode(err, domain.CodeConflict) {
		t.Fatalf("changed payload=%v", err)
	}
	c.Advance(30 * time.Minute)
	if _, err := s.Transcribe(context.Background(), domain.Device{ID: "d"}, req); err != nil || calls.Load() != 2 {
		t.Fatalf("expired ledger retry err=%v calls=%d", err, calls.Load())
	}

	bad := domain.TranscriptionRequest{RequestID: "bad", WAV: []byte("bad")}
	if _, err := s.Transcribe(context.Background(), domain.Device{ID: "d"}, bad); err == nil {
		t.Fatal("invalid WAV accepted")
	}
	if _, err := s.Transcribe(context.Background(), domain.Device{ID: "d"}, bad); err == nil {
		t.Fatal("invalid WAV error not replayed")
	}
	if _, err := s.Transcribe(context.Background(), domain.Device{}, req); err == nil {
		t.Fatal("empty device accepted")
	}
}

func TestServiceAdditionalTranscriptionPendingCancelProviderFailures(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	c := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := New(nil, transcriberFunc(func(context.Context, []byte) (string, error) {
		close(entered)
		<-release
		return "x", nil
	}), c, DefaultConfig())
	req := domain.TranscriptionRequest{RequestID: "pending", WAV: validWAV()}
	done := make(chan error, 1)
	go func() {
		_, err := s.Transcribe(context.Background(), domain.Device{ID: "d"}, req)
		done <- err
	}()
	<-entered
	if _, err := s.Transcribe(context.Background(), domain.Device{ID: "d"}, req); !isCode(err, domain.CodeInProgress) {
		t.Fatalf("pending replay=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	providerErr := New(nil, transcriberFunc(func(context.Context, []byte) (string, error) {
		return "", errors.New("secret provider body")
	}), c, DefaultConfig())
	if _, err := providerErr.Transcribe(context.Background(), domain.Device{ID: "e"}, domain.TranscriptionRequest{RequestID: "e", WAV: validWAV()}); !isCode(err, domain.CodeProviderUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("provider error=%v", err)
	}
	tooLong := New(nil, transcriberFunc(func(context.Context, []byte) (string, error) {
		return strings.Repeat("x", 4001), nil
	}), c, DefaultConfig())
	if _, err := tooLong.Transcribe(context.Background(), domain.Device{ID: "l"}, domain.TranscriptionRequest{RequestID: "l", WAV: validWAV()}); err == nil {
		t.Fatal("oversized transcript accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retryCalls := atomic.Int32{}
	retry := New(nil, transcriberFunc(func(context.Context, []byte) (string, error) {
		retryCalls.Add(1)
		return "ok", nil
	}), c, DefaultConfig())
	request := domain.TranscriptionRequest{RequestID: "retry", WAV: validWAV()}
	if _, err := retry.Transcribe(ctx, domain.Device{ID: "r"}, request); err == nil {
		t.Fatal("cancelled transcription accepted")
	}
	if _, err := retry.Transcribe(context.Background(), domain.Device{ID: "r"}, request); err != nil || retryCalls.Load() != 1 {
		t.Fatalf("safe retry err=%v calls=%d", err, retryCalls.Load())
	}
}

func TestServiceAdditionalExpiryCancellationAndUnsafeProviderAction(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s, c := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
		close(entered)
		<-release
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "late"}}, nil
	}))
	sess := create(t, s, "d")
	done := make(chan error, 1)
	go func() {
		_, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, turnReq("expiry", "x", "v"))
		done <- err
	}()
	<-entered
	c.Advance(30 * time.Minute)
	close(release)
	if err := <-done; !isCode(err, domain.CodeExpired) {
		t.Fatalf("late expiry=%v", err)
	}

	unsafeService, _ := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "do it", Action: &domain.ActionProposal{Kind: domain.ActionOpenApp, AppRef: "app-maps", AppLabel: "Карты", DisplayText: "Карты"}}}, nil
	}))
	unsafeSession := create(t, unsafeService, "d")
	if _, err := unsafeService.Turn(context.Background(), domain.Device{ID: "d"}, unsafeSession.ID, turnReq("unsafe", "что на экране", "v")); !isCode(err, domain.CodeUnsafe) {
		t.Fatalf("provider-created authority=%v", err)
	}
}

func TestServiceEmptyTreeStillAppliesDeterministicSafetyPolicy(t *testing.T) {
	s, _ := newHarness(nil)
	sess := create(t, s, "d")
	for i, tc := range []struct {
		text string
		kind domain.ReplyKind
	}{
		{"открой Карты", domain.ReplyActionProposal},
		{"позвони маме", domain.ReplyActionProposal},
		{"оплати счёт", domain.ReplyRefuse},
	} {
		req := turnReq(fmt.Sprintf("empty-%d", i), tc.text, "v")
		req.Screen.Nodes = nil
		got, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, req)
		if err != nil || got.Kind != tc.kind {
			t.Fatalf("%q: got=%q err=%v", tc.text, got.Kind, err)
		}
	}
}

func TestServiceIdempotencyHashDoesNotTreatInvalidBase64AsValidReplay(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	s, _ := newHarness(nil)
	sess := create(t, s, "d")
	req := turnReq("image-hash", "объясни экран", "v")
	req.Image = &domain.ImageInput{MediaType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(encoded.Bytes()), Consent: true, ScreenVersion: "v"}
	if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, req); err != nil {
		t.Fatal(err)
	}
	nonCanonical := cloneTurnRequest(req)
	nonCanonical.Image.DataBase64 += "\n"
	if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, nonCanonical); !isCode(err, domain.CodeConflict) {
		t.Fatalf("invalid base64 replay bypassed validation: %v", err)
	}
}

func TestServiceAdditionalReplayCopiesCancelAndExpiredGrantRecovery(t *testing.T) {
	s, c, sess, reply := actionHarness(t)
	originalID := reply.Action.ActionID
	reply.Action.ActionID = "mutated"
	replayed, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, turnReq("turn1", "позвони маме", "v1"))
	if err != nil || replayed.Action == nil || replayed.Action.ActionID != originalID {
		t.Fatalf("reply alias mutated stored value: %#v %v", replayed, err)
	}
	confirm := domain.ConfirmationRequest{RequestID: "confirm", ActionID: originalID, ScreenVersion: "v1", Generation: replayed.Generation, Decision: domain.DecisionConfirm}
	confirmed, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, confirm)
	if err != nil || confirmed.Grant == nil {
		t.Fatal(err)
	}
	confirmed.Grant.ActionID = "mutated"
	again, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, confirm)
	if err != nil || again.Grant == nil || again.Grant.ActionID != originalID {
		t.Fatalf("grant alias mutated replay: %#v %v", again, err)
	}
	c.Advance(30 * time.Second)
	if _, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, turnReq("after-grant", "позвони маме", "v2")); err != nil {
		t.Fatalf("expired grant blocked new turn: %v", err)
	}

	s2, _ := newHarness(nil)
	sess2 := create(t, s2, "d")
	cancelReq := domain.CancelRequest{RequestID: "cancel"}
	if err := s2.Cancel(context.Background(), domain.Device{ID: "d"}, sess2.ID, cancelReq); err != nil {
		t.Fatal(err)
	}
	if err := s2.Cancel(context.Background(), domain.Device{ID: "d"}, sess2.ID, cancelReq); err != nil {
		t.Fatalf("cancel replay=%v", err)
	}
	changed := cancelReq
	changed.RequestID = "other"
	if err := s2.Cancel(context.Background(), domain.Device{ID: "d"}, sess2.ID, changed); !isCode(err, domain.CodeConflict) {
		t.Fatalf("second cancel=%v", err)
	}
}

func TestServiceAdditionalHelperReplayBranches(t *testing.T) {
	h := [32]byte{1}
	other := [32]byte{2}
	pending := &ledgerEntry{hash: h, pending: true}
	if _, err := replayConfirmation(pending, h); !isCode(err, domain.CodeInProgress) {
		t.Fatal(err)
	}
	if err := replayVoid(pending, other); !isCode(err, domain.CodeConflict) {
		t.Fatal(err)
	}
	if _, err := replayTranscription(pending, other); !isCode(err, domain.CodeConflict) {
		t.Fatal(err)
	}
	if _, err := replayTranscription(pending, h); !isCode(err, domain.CodeInProgress) {
		t.Fatal(err)
	}
	failed := &ledgerEntry{hash: h, err: invalid("x")}
	if _, err := replayTranscription(failed, h); err == nil {
		t.Fatal("transcription error not replayed")
	}
	finished := &ledgerEntry{hash: h}
	finished.reply.Text = "ok"
	if out, err := replayTranscription(finished, h); err != nil || out.Text != "ok" {
		t.Fatalf("out=%#v err=%v", out, err)
	}
	var safeAppError *domain.AppError
	if !errors.As(safeError(errors.New("raw")), &safeAppError) || safeAppError.Code != domain.CodeInternal {
		t.Fatal("raw error leaked")
	}
	if internal("x") == nil {
		t.Fatal("internal helper")
	}

	s, _ := newHarness(nil)
	sess := create(t, s, "remove")
	turnEntry := &ledgerEntry{pending: true}
	s.mu.Lock()
	record := s.sessions[sess.ID]
	record.ledger["turn\x00remove"] = turnEntry
	record.generation = 7
	record.state = stateProcessing
	record.currentCancel = func() {}
	s.mu.Unlock()
	s.removeTurnReservation(sess.ID, "turn\x00remove", turnEntry, 7)
	s.mu.Lock()
	_, remains := record.ledger["turn\x00remove"]
	state := record.state
	s.mu.Unlock()
	if remains || state != stateActive {
		t.Fatalf("turn reservation remains=%v state=%v", remains, state)
	}

	transcriptionEntry := &ledgerEntry{pending: true}
	transcriptionLedger := &deviceLedger{expiresAt: time.Now().Add(time.Hour), entries: map[string]*ledgerEntry{"transcription\x00remove": transcriptionEntry}}
	s.mu.Lock()
	s.transcriptions["remove"] = transcriptionLedger
	s.mu.Unlock()
	s.removeTranscriptionReservation("remove", "transcription\x00remove", transcriptionEntry)
	s.mu.Lock()
	_, remains = transcriptionLedger.entries["transcription\x00remove"]
	s.mu.Unlock()
	if remains {
		t.Fatal("transcription reservation remains")
	}

	missing := New(nil, nil, &fakeClock{now: time.Now()}, DefaultConfig())
	if _, started, err := missing.callPlanner(context.Background(), domain.PlanInput{}); err == nil || started {
		t.Fatalf("nil planner started=%v err=%v", started, err)
	}
	if _, started, err := missing.callTranscriber(context.Background(), nil); err == nil || started {
		t.Fatalf("nil transcriber started=%v err=%v", started, err)
	}
}
