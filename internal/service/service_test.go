package service

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/100bench/OPORA/internal/domain"
)

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []clockWaiter
}
type clockWaiter struct {
	at time.Time
	ch chan time.Time
}
type observedContext struct {
	context.Context
	once     *sync.Once
	observed chan struct{}
}

func (c observedContext) mark()                 { c.once.Do(func() { close(c.observed) }) }
func (c observedContext) Done() <-chan struct{} { c.mark(); return c.Context.Done() }
func (c observedContext) Err() error            { c.mark(); return c.Context.Err() }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.waiters = append(c.waiters, clockWaiter{at: c.now.Add(d), ch: ch})
	c.mu.Unlock()
	return ch
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !c.now.Before(w.at) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
	c.mu.Unlock()
}

type plannerFunc func(context.Context, domain.PlanInput) (domain.PlanOutput, error)

func (f plannerFunc) Plan(c context.Context, i domain.PlanInput) (domain.PlanOutput, error) {
	return f(c, i)
}

type transcriberFunc func(context.Context, []byte) (string, error)

func (f transcriberFunc) Transcribe(c context.Context, b []byte) (string, error) { return f(c, b) }

func newHarness(p Planner) (*Service, *fakeClock) {
	c := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if p == nil {
		p = plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
			return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "ok"}}, nil
		})
	}
	return New(p, transcriberFunc(func(context.Context, []byte) (string, error) { return "тест", nil }), c, DefaultConfig()), c
}
func create(t *testing.T, s *Service, device string) domain.Session {
	t.Helper()
	x, err := s.CreateSession(context.Background(), domain.SessionConfig{DeviceID: device, Apps: []domain.App{{Ref: "app-maps", Label: "Карты"}}, Contacts: []domain.Contact{{Ref: "fav-mom", Label: "Мама"}}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return x
}
func turnReq(id, text, version string) domain.TurnRequest {
	return domain.TurnRequest{RequestID: id, Text: text, Screen: domain.ScreenSnapshot{Package: "ru.maps", Version: version, Width: 1080, Height: 1920, Nodes: []domain.ScreenNode{{ID: "b1", Text: "Маршрут", Visible: true, Enabled: true}}}}
}

func TestFR30_ScenarioMatrix(t *testing.T) {
	cases := []struct {
		name, text string
		apps       []domain.App
		contacts   []domain.Contact
		want       domain.ReplyKind
	}{
		{"open installed", "открой Карты", []domain.App{{Ref: "app-maps", Label: "Карты"}}, nil, domain.ReplyActionProposal},
		{"open missing", "открой Карты", nil, nil, domain.ReplyClarify},
		{"call configured", "позвони маме", nil, []domain.Contact{{Ref: "fav-mom", Label: "Мама"}}, domain.ReplyActionProposal},
		{"call ambiguous", "позвони маме", nil, []domain.Contact{{Ref: "a", Label: "Мама"}, {Ref: "b", Label: "Мама"}}, domain.ReplyClarify},
		{"highlight one", "покажи кнопку маршрута", nil, nil, domain.ReplyHighlight},
		{"highlight missing", "покажи оплату", nil, nil, domain.ReplyClarify},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newHarness(nil)
			sess, err := s.CreateSession(context.Background(), domain.SessionConfig{DeviceID: "d1", Apps: tc.apps, Contacts: tc.contacts})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			got, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, turnReq("r1", tc.text, "v1"))
			if err != nil {
				t.Fatalf("turn: %v", err)
			}
			if got.Kind != tc.want {
				t.Fatalf("kind=%q want %q", got.Kind, tc.want)
			}
		})
	}
}

func TestFR31_ImageConsentBindingAndBounds(t *testing.T) {
	s, _ := newHarness(nil)
	sess := create(t, s, "d1")
	cases := []struct {
		name     string
		img      *domain.ImageInput
		semantic bool
		want     domain.ReplyKind
	}{{"tree only explains", nil, true, domain.ReplyExplain}, {"insufficient tree asks image", nil, false, domain.ReplyNeedImage}, {"no consent refuses", &domain.ImageInput{MediaType: "image/png", DataBase64: "aQ==", ScreenVersion: "v1"}, false, domain.ReplyRefuse}, {"stale version refuses", &domain.ImageInput{MediaType: "image/png", DataBase64: "aQ==", Consent: true, ScreenVersion: "old"}, false, domain.ReplyRefuse}}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := turnReq(fmt.Sprintf("img-%d", i), "объясни экран", "v1")
			if !tc.semantic {
				r.Screen.Nodes = nil
			}
			r.Image = tc.img
			got, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, r)
			if err != nil {
				t.Fatalf("turn: %v", err)
			}
			if got.Kind != tc.want {
				t.Fatalf("got %q want %q", got.Kind, tc.want)
			}
		})
	}
}

func TestFR32_SensitiveImageRefusesBeforeProvider(t *testing.T) {
	for _, kind := range []string{"node", "screen-sensitive", "screen-protected"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			s, _ := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
				calls.Add(1)
				return domain.PlanOutput{}, nil
			}))
			sess := create(t, s, "d1")
			r := turnReq("sensitive-"+kind, "объясни экран", "v1")
			switch kind {
			case "node":
				r.Screen.Nodes[0].Sensitive = true
			case "screen-sensitive":
				r.Screen.Sensitive = true
			case "screen-protected":
				r.Screen.Protected = true
			}
			r.Image = &domain.ImageInput{MediaType: "image/png", DataBase64: "aQ==", Consent: true, ScreenVersion: "v1"}
			got, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, r)
			if err != nil {
				t.Fatalf("turn: %v", err)
			}
			if got.Kind != domain.ReplyRefuse {
				t.Fatalf("kind=%q want refuse", got.Kind)
			}
			if calls.Load() != 0 {
				t.Fatalf("provider called %d times", calls.Load())
			}
		})
	}
}

func actionHarnessKind(t *testing.T, kind domain.ActionKind) (*Service, *fakeClock, domain.Session, domain.Reply) {
	t.Helper()
	s, c := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
		a := &domain.ActionProposal{Kind: kind, DisplayText: "Действие"}
		if kind == domain.ActionPrepareDialer {
			a.ContactRef = "fav-mom"
		} else {
			a.AppRef = "app-maps"
			a.AppLabel = "Карты"
		}
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyActionProposal, Text: "Подтвердите действие", Action: a}}, nil
	}))
	sess := create(t, s, "d1")
	text := "позвони маме"
	if kind == domain.ActionOpenApp {
		text = "открой Карты"
	}
	reply, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, turnReq("turn1", text, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Action == nil || reply.Action.ActionID == "" || reply.Generation == 0 || reply.ScreenVersion != "v1" {
		t.Fatalf("missing issued proposal: %#v", reply)
	}
	return s, c, sess, reply
}
func actionHarness(t *testing.T) (*Service, *fakeClock, domain.Session, domain.Reply) {
	return actionHarnessKind(t, domain.ActionPrepareDialer)
}

func TestFR40_ConfirmationExpiryReplayTamperAndIsolation(t *testing.T) {
	t.Run("exact replay and one logical grant", func(t *testing.T) {
		s, _, sess, reply := actionHarness(t)
		req := domain.ConfirmationRequest{RequestID: "c1", ActionID: reply.Action.ActionID, ScreenVersion: reply.ScreenVersion, Generation: reply.Generation, Decision: domain.DecisionConfirm}
		first, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, req)
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, req)
		if err != nil || first.Grant == nil || again.Grant == nil || first.Grant.ActionID != again.Grant.ActionID {
			t.Fatalf("replay first=%#v again=%#v err=%v", first, again, err)
		}
		if first.Grant.Kind != reply.Action.Kind || first.Grant.ContactRef != reply.Action.ContactRef {
			t.Fatalf("grant changed proposal: %#v -> %#v", reply.Action, first.Grant)
		}
		changed := req
		changed.Decision = domain.DecisionDecline
		if _, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, changed); !isCode(err, domain.CodeConflict) {
			t.Fatalf("changed idempotency payload err=%v", err)
		}
		req.RequestID = "c2"
		if _, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, req); err == nil {
			t.Fatal("consumed proposal granted twice")
		}
	})
	t.Run("decline consumes without grant", func(t *testing.T) {
		s, _, sess, reply := actionHarness(t)
		req := domain.ConfirmationRequest{RequestID: "d1", ActionID: reply.Action.ActionID, ScreenVersion: reply.ScreenVersion, Generation: reply.Generation, Decision: domain.DecisionDecline}
		out, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, req)
		if err != nil || out.Status != "declined" || out.Grant != nil {
			t.Fatalf("decline=%#v err=%v", out, err)
		}
		replay, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, req)
		if err != nil || replay.Status != "declined" || replay.Grant != nil {
			t.Fatalf("decline replay=%#v err=%v", replay, err)
		}
		req.RequestID = "d2"
		req.Decision = domain.DecisionConfirm
		if _, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, req); err == nil {
			t.Fatal("declined proposal revived")
		}
	})
	for _, name := range []string{"screen", "generation", "action", "device", "session", "expiry"} {
		t.Run(name, func(t *testing.T) {
			s, c, sess, reply := actionHarness(t)
			req := domain.ConfirmationRequest{RequestID: "bad-" + name, ActionID: reply.Action.ActionID, ScreenVersion: reply.ScreenVersion, Generation: reply.Generation, Decision: domain.DecisionConfirm}
			dev := domain.Device{ID: "d1"}
			sid := sess.ID
			switch name {
			case "screen":
				req.ScreenVersion = "old"
			case "generation":
				req.Generation++
			case "action":
				req.ActionID = "tampered"
			case "device":
				dev.ID = "d2"
			case "session":
				sid = "other"
			case "expiry":
				c.Advance(30 * time.Second)
			}
			if _, err := s.Confirm(context.Background(), dev, sid, req); err == nil {
				t.Fatalf("%s tamper accepted", name)
			}
		})
	}
	t.Run("just before expiry valid", func(t *testing.T) {
		s, c, sess, reply := actionHarness(t)
		c.Advance(30*time.Second - time.Millisecond)
		req := domain.ConfirmationRequest{RequestID: "edge", ActionID: reply.Action.ActionID, ScreenVersion: reply.ScreenVersion, Generation: reply.Generation, Decision: domain.DecisionConfirm}
		if _, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, req); err != nil {
			t.Fatalf("29.999s confirmation: %v", err)
		}
	})
}

func TestFR41_ResultOnlyForIssuedActionAndNoConnectedUpgrade(t *testing.T) {
	issue := func(t *testing.T, kind domain.ActionKind) (*Service, domain.Session, string) {
		s, _, sess, reply := actionHarnessKind(t, kind)
		out, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.ConfirmationRequest{RequestID: "c", ActionID: reply.Action.ActionID, ScreenVersion: reply.ScreenVersion, Generation: reply.Generation, Decision: domain.DecisionConfirm})
		if err != nil || out.Grant == nil {
			t.Fatalf("issue: %#v %v", out, err)
		}
		return s, sess, out.Grant.ActionID
	}
	for _, status := range []string{"dialer_presented", "failed", "cancelled"} {
		t.Run("valid_"+status, func(t *testing.T) {
			s, sess, id := issue(t, domain.ActionPrepareDialer)
			if err := s.ReportResult(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.ActionResultRequest{RequestID: "r", ActionID: id, Status: status}); err != nil {
				t.Fatalf("valid result: %v", err)
			}
		})
	}
	t.Run("valid_opened_and_replay", func(t *testing.T) {
		s, sess, id := issue(t, domain.ActionOpenApp)
		req := domain.ActionResultRequest{RequestID: "open-result", ActionID: id, Status: "opened"}
		if err := s.ReportResult(context.Background(), domain.Device{ID: "d1"}, sess.ID, req); err != nil {
			t.Fatal(err)
		}
		if err := s.ReportResult(context.Background(), domain.Device{ID: "d1"}, sess.ID, req); err != nil {
			t.Fatalf("replay: %v", err)
		}
	})
	for _, tc := range []struct {
		name, status                           string
		wrongDevice, wrongSession, wrongAction bool
	}{{"connected upgrade", "call_connected", false, false, false}, {"kind mismatch", "opened", false, false, false}, {"unissued", "failed", false, false, true}, {"cross device", "failed", true, false, false}, {"cross session", "failed", false, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, sess, id := issue(t, domain.ActionPrepareDialer)
			dev := domain.Device{ID: "d1"}
			sid := sess.ID
			if tc.wrongDevice {
				dev.ID = "d2"
			}
			if tc.wrongSession {
				sid = "other"
			}
			if tc.wrongAction {
				id = "unknown"
			}
			if err := s.ReportResult(context.Background(), dev, sid, domain.ActionResultRequest{RequestID: "bad", ActionID: id, Status: tc.status}); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
}

func TestNFR10_IdempotencySameDifferentAndConcurrent(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s, _ := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "Экран открыт"}}, nil
	}))
	sess := create(t, s, "d1")
	req := turnReq("same", "что это?", "v1")
	errs := make(chan error, 1)
	go func() { _, e := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, req); errs <- e }()
	<-started
	_, concurrentErr := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, req)
	close(release)
	firstErr := <-errs
	ok, progress := 0, 0
	if firstErr == nil {
		ok++
	}
	var ae *domain.AppError
	if errors.As(concurrentErr, &ae) && ae.Code == domain.CodeInProgress {
		progress++
	}
	if ok != 1 || progress != 1 || calls.Load() != 1 {
		t.Fatalf("ok=%d in_progress=%d calls=%d", ok, progress, calls.Load())
	}
	changed := req
	changed.Text = "другое"
	if _, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, changed); err == nil {
		t.Error("same key with different payload accepted")
	}
}

func TestNFR10b_IdempotencyCompletedSuccessErrorAndSafeAbort(t *testing.T) {
	t.Run("success replay", func(t *testing.T) {
		var calls atomic.Int32
		s, _ := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
			calls.Add(1)
			return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "same"}}, nil
		}))
		sess := create(t, s, "d")
		req := turnReq("r", "x", "v")
		first, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, req)
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, req)
		if err != nil || again.Text != first.Text || calls.Load() != 1 {
			t.Fatalf("replay=%#v err=%v calls=%d", again, err, calls.Load())
		}
	})
	t.Run("error replay", func(t *testing.T) {
		var calls atomic.Int32
		s, _ := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
			calls.Add(1)
			return domain.PlanOutput{}, errors.New("boom")
		}))
		sess := create(t, s, "d")
		req := turnReq("r", "x", "v")
		_, e1 := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, req)
		_, e2 := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, req)
		if e1 == nil || e2 == nil || calls.Load() != 1 {
			t.Fatalf("errors=%v/%v calls=%d", e1, e2, calls.Load())
		}
	})
	t.Run("pre-provider cancel releases reservation", func(t *testing.T) {
		var calls atomic.Int32
		s, _ := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
			calls.Add(1)
			return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "Экран открыт"}}, nil
		}))
		sess := create(t, s, "d")
		req := turnReq("r", "x", "v")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.Turn(ctx, domain.Device{ID: "d"}, sess.ID, req); err == nil {
			t.Fatal("cancelled request accepted")
		}
		if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, req); err != nil {
			t.Fatalf("safe retry: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("provider calls=%d", calls.Load())
		}
	})
}

func TestNFR11_NewTurnCancelAndLateProviderCannotPublish(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s, _ := newHarness(plannerFunc(func(_ context.Context, _ domain.PlanInput) (domain.PlanOutput, error) {
		close(entered)
		<-release
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "late"}}, nil
	}))
	sess := create(t, s, "d1")
	done := make(chan error, 1)
	go func() {
		_, e := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, turnReq("r1", "первый", "v1"))
		done <- e
	}()
	<-entered
	if err := s.Cancel(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.CancelRequest{RequestID: "cancel1"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("late provider result published after cancel")
	}
	if _, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, turnReq("r2", "новый", "v2")); err == nil {
		t.Fatal("cancelled session resurrected")
	}
}

func TestNFR11b_NewTurnInvalidatesOlderGeneration(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s, _ := newHarness(plannerFunc(func(_ context.Context, in domain.PlanInput) (domain.PlanOutput, error) {
		if in.Text == "первый" {
			close(entered)
			<-release
		}
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: in.Text}}, nil
	}))
	sess := create(t, s, "d1")
	old := make(chan error, 1)
	go func() {
		_, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, turnReq("old", "первый", "v1"))
		old <- err
	}()
	<-entered
	fresh, err := s.Turn(context.Background(), domain.Device{ID: "d1"}, sess.ID, turnReq("new", "второй", "v2"))
	if err != nil {
		t.Fatalf("new turn: %v", err)
	}
	close(release)
	if err := <-old; err == nil {
		t.Fatal("older generation published")
	}
	if fresh.ScreenVersion != "v2" {
		t.Fatalf("fresh reply version=%q", fresh.ScreenVersion)
	}
}

func TestNFR12_SessionLimitTTLAndNoResurrection(t *testing.T) {
	t.Run("exact ttl and idle refresh", func(t *testing.T) {
		s, c := newHarness(nil)
		sess := create(t, s, "d")
		c.Advance(29*time.Minute + 59*time.Second)
		if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, turnReq("refresh", "ok", "v")); err != nil {
			t.Fatalf("before ttl: %v", err)
		}
		c.Advance(29*time.Minute + 59*time.Second)
		if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, turnReq("still-live", "ok", "v2")); err != nil {
			t.Fatalf("refresh failed: %v", err)
		}
		c.Advance(30 * time.Minute)
		if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, turnReq("expired", "ok", "v3")); err == nil {
			t.Fatal("exactly expired session accepted")
		}
		if err := s.Cancel(context.Background(), domain.Device{ID: "d"}, sess.ID, domain.CancelRequest{RequestID: "revive"}); err == nil {
			t.Fatal("expired session resurrected")
		}
	})
	t.Run("limit and cleanup", func(t *testing.T) {
		s, c := newHarness(nil)
		ids := make([]string, 100)
		for i := range ids {
			x, err := s.CreateSession(context.Background(), domain.SessionConfig{DeviceID: "d"})
			if err != nil {
				t.Fatalf("session %d: %v", i, err)
			}
			ids[i] = x.ID
		}
		if _, err := s.CreateSession(context.Background(), domain.SessionConfig{DeviceID: "d"}); err == nil {
			t.Fatal("101st session accepted")
		}
		c.Advance(30 * time.Minute)
		fresh, err := s.CreateSession(context.Background(), domain.SessionConfig{DeviceID: "d"})
		if err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, ids[0], turnReq("old", "x", "v")); err == nil {
			t.Fatal("cleaned id resurrected")
		}
		if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, fresh.ID, turnReq("fresh", "x", "v")); err != nil {
			t.Fatalf("fresh: %v", err)
		}
	})
}

func TestNFR12b_DeviceIsolationAllSessionEndpoints(t *testing.T) {
	other := domain.Device{ID: "d2"}
	t.Run("turn", func(t *testing.T) {
		s, _, sess, _ := actionHarness(t)
		if _, err := s.Turn(context.Background(), other, sess.ID, turnReq("foreign-turn", "x", "v2")); !isCode(err, domain.CodeNotFound) {
			t.Fatalf("foreign turn err=%v", err)
		}
	})
	t.Run("confirm and result", func(t *testing.T) {
		s, _, sess, reply := actionHarness(t)
		req := domain.ConfirmationRequest{RequestID: "owner-confirm", ActionID: reply.Action.ActionID, ScreenVersion: reply.ScreenVersion, Generation: reply.Generation, Decision: domain.DecisionConfirm}
		foreign := req
		foreign.RequestID = "foreign-confirm"
		if _, err := s.Confirm(context.Background(), other, sess.ID, foreign); !isCode(err, domain.CodeNotFound) {
			t.Fatalf("foreign confirm err=%v", err)
		}
		out, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, req)
		if err != nil || out.Grant == nil {
			t.Fatalf("owner confirm=%#v err=%v", out, err)
		}
		result := domain.ActionResultRequest{RequestID: "owner-result", ActionID: out.Grant.ActionID, Status: "dialer_presented"}
		foreignResult := result
		foreignResult.RequestID = "foreign-result"
		if err := s.ReportResult(context.Background(), other, sess.ID, foreignResult); !isCode(err, domain.CodeNotFound) {
			t.Fatalf("foreign result err=%v", err)
		}
		if err := s.ReportResult(context.Background(), domain.Device{ID: "d1"}, sess.ID, result); err != nil {
			t.Fatalf("owner result: %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		s, _, sess, _ := actionHarness(t)
		if err := s.Cancel(context.Background(), other, sess.ID, domain.CancelRequest{RequestID: "foreign-cancel"}); !isCode(err, domain.CodeNotFound) {
			t.Fatalf("foreign cancel err=%v", err)
		}
		if err := s.Cancel(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.CancelRequest{RequestID: "owner-cancel"}); err != nil {
			t.Fatalf("owner cancel: %v", err)
		}
	})
}

func TestNFR13_ProviderCapQueuesFifthAndReleasesOnExit(t *testing.T) {
	var active, maxActive atomic.Int32
	gate := make(chan struct{})
	entered := make(chan struct{}, 5)
	var providerWG sync.WaitGroup
	providerWG.Add(5)
	p := plannerFunc(func(_ context.Context, _ domain.PlanInput) (domain.PlanOutput, error) {
		defer providerWG.Done()
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		entered <- struct{}{}
		defer active.Add(-1)
		<-gate
		return domain.PlanOutput{}, errors.New("provider error")
	})
	s, _ := newHarness(p)
	sessions := make([]domain.Session, 5)
	for i := range sessions {
		sessions[i] = create(t, s, "d")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.Turn(context.Background(), domain.Device{ID: "d"}, sessions[i].ID, turnReq(fmt.Sprintf("call-%d", i), "x", "v"))
		}(i)
	}
	for range 4 {
		<-entered
	}
	if maxActive.Load() != 4 || active.Load() != 4 {
		t.Fatalf("provider max/active=%d/%d want 4/4", maxActive.Load(), active.Load())
	}
	if err := s.Cancel(context.Background(), domain.Device{ID: "d"}, sessions[0].ID, domain.CancelRequest{RequestID: "cancel-running"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if active.Load() != 4 {
		t.Fatalf("cancel released permit before provider exit: active=%d", active.Load())
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan struct{})
	ctx := observedContext{Context: base, once: &sync.Once{}, observed: observed}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.Turn(ctx, domain.Device{ID: "d"}, sessions[4].ID, turnReq("fifth", "x", "v"))
	}()
	<-observed
	select {
	case <-entered:
		t.Fatal("fifth provider entered before permit release")
	default:
	}
	gate <- struct{}{}
	<-entered
	close(gate)
	wg.Wait()
	providerWG.Wait()
	if active.Load() != 0 {
		t.Fatalf("provider permits leaked: %d", active.Load())
	}
}

func TestNFR13a_GlobalCapSharedByPlannerAndTranscriber(t *testing.T) {
	entered := make(chan struct{}, 5)
	release := make(chan struct{})
	block := func(context.Context) { entered <- struct{}{}; <-release }
	p := plannerFunc(func(ctx context.Context, _ domain.PlanInput) (domain.PlanOutput, error) {
		block(ctx)
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "ok"}}, nil
	})
	tr := transcriberFunc(func(ctx context.Context, _ []byte) (string, error) { block(ctx); return "ok", nil })
	c := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s := New(p, tr, c, DefaultConfig())
	a := create(t, s, "d")
	b := create(t, s, "d")
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		_, _ = s.Turn(context.Background(), domain.Device{ID: "d"}, a.ID, turnReq("p1", "x", "v"))
	}()
	go func() {
		defer wg.Done()
		_, _ = s.Turn(context.Background(), domain.Device{ID: "d"}, b.ID, turnReq("p2", "x", "v"))
	}()
	go func() {
		defer wg.Done()
		_, _ = s.Transcribe(context.Background(), domain.Device{ID: "d"}, domain.TranscriptionRequest{RequestID: "t1", WAV: validWAV()})
	}()
	go func() {
		defer wg.Done()
		_, _ = s.Transcribe(context.Background(), domain.Device{ID: "d"}, domain.TranscriptionRequest{RequestID: "t2", WAV: validWAV()})
	}()
	for range 4 {
		<-entered
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan struct{})
	ctx := observedContext{Context: base, once: &sync.Once{}, observed: observed}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.Transcribe(ctx, domain.Device{ID: "d"}, domain.TranscriptionRequest{RequestID: "t3", WAV: validWAV()})
	}()
	<-observed
	select {
	case <-entered:
		t.Fatal("fifth mixed provider call entered")
	default:
	}
	release <- struct{}{}
	<-entered
	close(release)
	wg.Wait()
}

func TestNFR13b_ProviderTimeoutUsesInjectedClockAndReleasesPermit(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	s, c := newHarness(plannerFunc(func(ctx context.Context, _ domain.PlanInput) (domain.PlanOutput, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return domain.PlanOutput{}, ctx.Err()
		}
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "ok"}}, nil
	}))
	sess := create(t, s, "d")
	done := make(chan error, 1)
	go func() {
		_, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, turnReq("timeout", "x", "v"))
		done <- err
	}()
	<-entered
	c.Advance(19*time.Second + 999*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("timed out early: %v", err)
	default:
	}
	c.Advance(time.Millisecond)
	if err := <-done; !isCode(err, domain.CodeProviderUnavailable) {
		t.Fatalf("timeout err=%v", err)
	}
	fresh := create(t, s, "d")
	if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, fresh.ID, turnReq("after-timeout", "x", "v")); err != nil {
		t.Fatalf("permit not released: %v", err)
	}
}

func TestNFR13c_NoSessionLockHeldAcrossProviderIO(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s, _ := newHarness(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
		close(entered)
		<-release
		return domain.PlanOutput{Reply: domain.Reply{Kind: domain.ReplyExplain, Text: "ok"}}, nil
	}))
	sess := create(t, s, "d")
	done := make(chan struct{})
	go func() {
		_, _ = s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, turnReq("blocked", "x", "v"))
		close(done)
	}()
	<-entered
	if _, err := s.CreateSession(context.Background(), domain.SessionConfig{DeviceID: "other"}); err != nil {
		t.Fatalf("unrelated state blocked: %v", err)
	}
	close(release)
	<-done
}

func TestNFR14_RequestLedgersAreBounded(t *testing.T) {
	s, _ := newHarness(nil)
	sess := create(t, s, "d")
	for i := 0; i < 128; i++ {
		id := fmt.Sprintf("turn-%03d", i)
		if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, turnReq(id, "x", "v")); err != nil {
			t.Fatalf("operation %d: %v", i, err)
		}
	}
	if _, err := s.Turn(context.Background(), domain.Device{ID: "d"}, sess.ID, turnReq("overflow", "x", "v")); !isCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("129th ledger err=%v", err)
	}
	w := validWAV()
	for i := 0; i < 128; i++ {
		id := fmt.Sprintf("audio-%03d", i)
		if _, err := s.Transcribe(context.Background(), domain.Device{ID: "voice"}, domain.TranscriptionRequest{RequestID: id, WAV: w}); err != nil {
			t.Fatalf("transcription %d: %v", i, err)
		}
	}
	if _, err := s.Transcribe(context.Background(), domain.Device{ID: "voice"}, domain.TranscriptionRequest{RequestID: "overflow", WAV: w}); !isCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("129th transcription ledger err=%v", err)
	}
}

func isCode(err error, code domain.ErrorCode) bool {
	var ae *domain.AppError
	return errors.As(err, &ae) && ae.Code == code
}
func validWAV() []byte {
	b := make([]byte, 46)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 38)
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 2)
	return b
}
