package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/100bench/OPORA/internal/domain"
)

func TestNFR10_ExpiredTranscriptionCompletionCannotOverwriteReplacementLedger(t *testing.T) {
	s, clock := newHarness(nil)
	const (
		deviceID = "device"
		key      = "transcription\x00same"
	)
	oldEntry := &ledgerEntry{pending: true}
	oldLedger := &deviceLedger{
		expiresAt: clock.Now().Add(s.cfg.SessionTTL),
		entries:   map[string]*ledgerEntry{key: oldEntry},
	}
	s.mu.Lock()
	s.transcriptions[deviceID] = oldLedger
	s.mu.Unlock()

	clock.Advance(s.cfg.SessionTTL)
	replacementEntry := &ledgerEntry{pending: true}
	replacementEntry.reply.Text = "fresh"
	replacementLedger := &deviceLedger{
		expiresAt: clock.Now().Add(s.cfg.SessionTTL),
		entries:   map[string]*ledgerEntry{key: replacementEntry},
	}
	s.mu.Lock()
	s.transcriptions[deviceID] = replacementLedger
	s.mu.Unlock()

	if out, err := s.completeTranscription(deviceID, key, oldLedger, oldEntry, domain.Transcription{Text: "late-old"}, nil); !isCode(err, domain.CodeConflict) || out.Text != "" {
		t.Fatalf("late completion out=%#v err=%v", out, err)
	}
	s.mu.Lock()
	gotLedger := s.transcriptions[deviceID]
	gotEntry := gotLedger.entries[key]
	s.mu.Unlock()
	if gotLedger != replacementLedger || gotEntry != replacementEntry || !gotEntry.pending || gotEntry.reply.Text != "fresh" {
		t.Fatalf("replacement ledger was mutated: ledger=%p entry=%#v", gotLedger, gotEntry)
	}
}

func TestNFR11_ResultCancelRaceIsTerminalAndCannotRevive(t *testing.T) {
	t.Run("successful result overlapping cancel", func(t *testing.T) {
		s, sess, actionID := issuedDialerAction(t)
		clock := newOrderedNowClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		s.clock = clock

		resultDone := make(chan error, 1)
		cancelDone := make(chan error, 1)
		s.mu.Lock()
		go func() {
			resultDone <- s.ReportResult(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.ActionResultRequest{RequestID: "result", ActionID: actionID, Status: "dialer_presented"})
		}()
		<-clock.firstReturned
		go func() {
			cancelDone <- s.Cancel(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.CancelRequest{RequestID: "cancel"})
		}()
		<-clock.secondEntered
		s.mu.Unlock()

		if err := <-resultDone; err != nil {
			t.Fatalf("result did not succeed first: %v", err)
		}
		close(clock.releaseSecond)
		if err := <-cancelDone; err != nil {
			t.Fatalf("overlapping cancel: %v", err)
		}
		assertSessionCancelled(t, s, sess.ID)
	})

	t.Run("cancel first rejects overlapping result", func(t *testing.T) {
		s, sess, actionID := issuedDialerAction(t)
		clock := newOrderedNowClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		s.clock = clock

		cancelDone := make(chan error, 1)
		resultDone := make(chan error, 1)
		s.mu.Lock()
		go func() {
			cancelDone <- s.Cancel(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.CancelRequest{RequestID: "cancel"})
		}()
		<-clock.firstReturned
		go func() {
			resultDone <- s.ReportResult(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.ActionResultRequest{RequestID: "result", ActionID: actionID, Status: "dialer_presented"})
		}()
		<-clock.secondEntered
		s.mu.Unlock()

		if err := <-cancelDone; err != nil {
			t.Fatalf("cancel did not succeed first: %v", err)
		}
		close(clock.releaseSecond)
		if err := <-resultDone; !isCode(err, domain.CodeConflict) {
			t.Fatalf("result after terminal cancel: %v", err)
		}
		assertSessionCancelled(t, s, sess.ID)
	})
}

func TestNFR13_PostAdmissionGuardsDoNotInvokeProviderOrLeakSlot(t *testing.T) {
	t.Run("planner cancelled after admission", func(t *testing.T) {
		var calls atomic.Int32
		s := New(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
			calls.Add(1)
			return domain.PlanOutput{}, nil
		}), nil, &stepClock{times: []time.Time{time.Unix(0, 0)}}, DefaultConfig())
		base, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx := &cancelOnErrContext{Context: base, cancel: cancel}
		_, started, err := s.callPlanner(ctx, domain.PlanInput{})
		assertProviderNotStartedAndSlotReleased(t, started, err, domain.CodeConflict, calls.Load(), s)
	})

	t.Run("transcriber cancelled after admission", func(t *testing.T) {
		var calls atomic.Int32
		s := New(nil, transcriberFunc(func(context.Context, []byte) (string, error) {
			calls.Add(1)
			return "", nil
		}), &stepClock{times: []time.Time{time.Unix(0, 0)}}, DefaultConfig())
		base, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx := &cancelOnErrContext{Context: base, cancel: cancel}
		_, started, err := s.callTranscriber(ctx, nil)
		assertProviderNotStartedAndSlotReleased(t, started, err, domain.CodeConflict, calls.Load(), s)
	})

	t.Run("planner deadline reached after admission", func(t *testing.T) {
		var calls atomic.Int32
		start := time.Unix(0, 0)
		s := New(plannerFunc(func(context.Context, domain.PlanInput) (domain.PlanOutput, error) {
			calls.Add(1)
			return domain.PlanOutput{}, nil
		}), nil, &stepClock{times: []time.Time{start, start.Add(DefaultConfig().ProviderTimeout)}}, DefaultConfig())
		_, started, err := s.callPlanner(context.Background(), domain.PlanInput{})
		assertProviderNotStartedAndSlotReleased(t, started, err, domain.CodeProviderUnavailable, calls.Load(), s)
	})

	t.Run("transcriber deadline reached after admission", func(t *testing.T) {
		var calls atomic.Int32
		start := time.Unix(0, 0)
		s := New(nil, transcriberFunc(func(context.Context, []byte) (string, error) {
			calls.Add(1)
			return "", nil
		}), &stepClock{times: []time.Time{start, start.Add(DefaultConfig().ProviderTimeout)}}, DefaultConfig())
		_, started, err := s.callTranscriber(context.Background(), nil)
		assertProviderNotStartedAndSlotReleased(t, started, err, domain.CodeProviderUnavailable, calls.Load(), s)
	})
}

func issuedDialerAction(t *testing.T) (*Service, domain.Session, string) {
	t.Helper()
	s, _, sess, reply := actionHarness(t)
	out, err := s.Confirm(context.Background(), domain.Device{ID: "d1"}, sess.ID, domain.ConfirmationRequest{
		RequestID:     "confirm",
		ActionID:      reply.Action.ActionID,
		ScreenVersion: reply.ScreenVersion,
		Generation:    reply.Generation,
		Decision:      domain.DecisionConfirm,
	})
	if err != nil || out.Grant == nil {
		t.Fatalf("issue action: %#v %v", out, err)
	}
	return s, sess, out.Grant.ActionID
}

func assertSessionCancelled(t *testing.T, s *Service, sessionID string) {
	t.Helper()
	s.mu.Lock()
	state := s.sessions[sessionID].state
	s.mu.Unlock()
	if state != stateCancelled {
		t.Fatalf("session state=%v want cancelled", state)
	}
}

func assertProviderNotStartedAndSlotReleased(t *testing.T, started bool, err error, code domain.ErrorCode, calls int32, s *Service) {
	t.Helper()
	if started || !isCode(err, code) || calls != 0 || len(s.providerSlots) != 0 {
		t.Fatalf("started=%v err=%v calls=%d held_slots=%d", started, err, calls, len(s.providerSlots))
	}
}

type orderedNowClock struct {
	now           time.Time
	calls         atomic.Int32
	firstReturned chan struct{}
	secondEntered chan struct{}
	releaseSecond chan struct{}
	firstOnce     sync.Once
	secondOnce    sync.Once
}

func newOrderedNowClock(now time.Time) *orderedNowClock {
	return &orderedNowClock{
		now:           now,
		firstReturned: make(chan struct{}),
		secondEntered: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
}

func (c *orderedNowClock) Now() time.Time {
	switch c.calls.Add(1) {
	case 1:
		c.firstOnce.Do(func() { close(c.firstReturned) })
	case 2:
		c.secondOnce.Do(func() { close(c.secondEntered) })
		<-c.releaseSecond
	}
	return c.now
}

func (*orderedNowClock) After(time.Duration) <-chan time.Time {
	return make(chan time.Time)
}

type cancelOnErrContext struct {
	context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnErrContext) Err() error {
	c.once.Do(c.cancel)
	return c.Context.Err()
}

type stepClock struct {
	mu    sync.Mutex
	times []time.Time
	next  int
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	now := c.times[c.next]
	c.next++
	return now
}

func (*stepClock) After(time.Duration) <-chan time.Time {
	return make(chan time.Time)
}
