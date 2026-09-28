package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/100bench/OPORA/internal/domain"
	"github.com/100bench/OPORA/internal/policy"
	"github.com/100bench/OPORA/internal/validation"
)

// Service is the in-memory implementation of Assistant.
type Service struct {
	planner     Planner
	transcriber Transcriber
	clock       Clock
	cfg         Config

	mu             sync.Mutex
	sessions       map[string]*sessionRecord
	transcriptions map[string]*deviceLedger
	providerSlots  chan struct{}
	idFallback     atomic.Uint64
}

// New constructs an isolated in-memory assistant service.
func New(planner Planner, transcriber Transcriber, clock Clock, cfg Config) *Service {
	cfg = boundedConfig(cfg)
	return &Service{
		planner:        planner,
		transcriber:    transcriber,
		clock:          clock,
		cfg:            cfg,
		sessions:       make(map[string]*sessionRecord),
		transcriptions: make(map[string]*deviceLedger),
		providerSlots:  make(chan struct{}, cfg.MaxProviderCalls),
	}
}

type sessionState uint8

const (
	stateActive sessionState = iota
	stateProcessing
	stateAwaitingConfirmation
	stateActionIssued
	stateCancelled
)

const maxLedgerEntries = 128

type sessionRecord struct {
	id             string
	deviceID       string
	apps           []domain.App
	contacts       []domain.Contact
	expiresAt      time.Time
	generation     uint64
	state          sessionState
	currentCancel  context.CancelFunc
	proposal       *proposalBinding
	grant          *grantBinding
	resultRecorded bool
	ledger         map[string]*ledgerEntry
}

type proposalBinding struct {
	action        domain.ActionProposal
	actionHash    [32]byte
	generation    uint64
	screenVersion string
	expiresAt     time.Time
}

type grantBinding struct {
	grant     domain.ActionGrant
	expiresAt time.Time
}

type ledgerEntry struct {
	hash         [32]byte
	pending      bool
	reply        domain.Reply
	confirmation domain.ConfirmationResult
	err          error
}

type deviceLedger struct {
	expiresAt time.Time
	entries   map[string]*ledgerEntry
}

// CreateSession creates a bounded volatile session for one authenticated device.
func (s *Service) CreateSession(ctx context.Context, cfg domain.SessionConfig) (domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return domain.Session{}, cancelled()
	}
	if s.clock == nil {
		return domain.Session{}, internal("service clock unavailable")
	}
	cfg.Apps = cloneApps(cfg.Apps)
	cfg.Contacts = cloneContacts(cfg.Contacts)
	if err := validateSessionConfig(cfg); err != nil {
		return domain.Session{}, err
	}
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupSessionsLocked(now)
	if len(s.sessions) >= s.cfg.MaxSessions {
		return domain.Session{}, resourceExhausted("session limit reached")
	}
	id := s.newID("sess")
	expires := now.Add(s.cfg.SessionTTL)
	s.sessions[id] = &sessionRecord{
		id:        id,
		deviceID:  cfg.DeviceID,
		apps:      cloneApps(cfg.Apps),
		contacts:  cloneContacts(cfg.Contacts),
		expiresAt: expires,
		state:     stateActive,
		ledger:    make(map[string]*ledgerEntry),
	}
	return domain.Session{ID: id, ExpiresAt: expires}, nil
}

// Transcribe validates and idempotently transcribes one WAV payload.
func (s *Service) Transcribe(ctx context.Context, device domain.Device, req domain.TranscriptionRequest) (domain.Transcription, error) {
	req.WAV = append([]byte(nil), req.WAV...)
	if err := validation.RequestID(req.RequestID); err != nil {
		return domain.Transcription{}, err
	}
	if device.ID == "" {
		return domain.Transcription{}, notFound()
	}
	hash := transcriptionHash(req)
	key := ledgerKey("transcription", req.RequestID)
	now := s.clock.Now()
	s.mu.Lock()
	s.cleanupTranscriptionsLocked(now)
	ledger := s.transcriptions[device.ID]
	if ledger == nil {
		ledger = &deviceLedger{expiresAt: now.Add(s.cfg.SessionTTL), entries: make(map[string]*ledgerEntry)}
		s.transcriptions[device.ID] = ledger
	}
	if entry, ok := ledger.entries[key]; ok {
		ledger.expiresAt = now.Add(s.cfg.SessionTTL)
		out, err := replayTranscription(entry, hash)
		s.mu.Unlock()
		return out, err
	}
	if len(ledger.entries) >= maxLedgerEntries {
		s.mu.Unlock()
		return domain.Transcription{}, resourceExhausted("transcription request ledger is full")
	}
	if ctx.Err() != nil {
		s.mu.Unlock()
		return domain.Transcription{}, cancelled()
	}
	entry := &ledgerEntry{hash: hash, pending: true}
	ledger.entries[key] = entry
	ledger.expiresAt = now.Add(s.cfg.SessionTTL)
	s.mu.Unlock()

	if err := validation.WAV(req.WAV); err != nil {
		return s.completeTranscription(device.ID, key, ledger, entry, domain.Transcription{}, err)
	}
	if ctx.Err() != nil {
		s.removeTranscriptionReservation(device.ID, key, entry)
		return domain.Transcription{}, cancelled()
	}
	text, started, err := s.callTranscriber(ctx, req.WAV)
	if err != nil {
		if !started && ctx.Err() != nil {
			s.removeTranscriptionReservation(device.ID, key, entry)
			return domain.Transcription{}, cancelled()
		}
		return s.completeTranscription(device.ID, key, ledger, entry, domain.Transcription{}, err)
	}
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 4000 {
		return s.completeTranscription(device.ID, key, ledger, entry, domain.Transcription{}, invalid("invalid transcription output"))
	}
	return s.completeTranscription(device.ID, key, ledger, entry, domain.Transcription{Text: text}, nil)
}

// Turn processes one generation-bound user turn.
func (s *Service) Turn(ctx context.Context, device domain.Device, sessionID string, req domain.TurnRequest) (domain.Reply, error) {
	req = cloneTurnRequest(req)
	if err := validation.RequestID(req.RequestID); err != nil {
		return domain.Reply{}, err
	}
	hash := turnHash(req)
	key := ledgerKey("turn", req.RequestID)
	now := s.clock.Now()
	s.mu.Lock()
	s.cleanupSessionsLocked(now)
	sess, err := s.sessionLocked(device.ID, sessionID)
	if err != nil {
		s.mu.Unlock()
		return domain.Reply{}, err
	}
	if entry, ok := sess.ledger[key]; ok {
		sess.expiresAt = now.Add(s.cfg.SessionTTL)
		out, replayErr := replayReply(entry, hash)
		s.mu.Unlock()
		return out, replayErr
	}
	if len(sess.ledger) >= maxLedgerEntries {
		s.mu.Unlock()
		return domain.Reply{}, resourceExhausted("session request ledger is full")
	}
	if ctx.Err() != nil {
		s.mu.Unlock()
		return domain.Reply{}, cancelled()
	}
	entry := &ledgerEntry{hash: hash, pending: true}
	sess.ledger[key] = entry
	sess.expiresAt = now.Add(s.cfg.SessionTTL)
	if sess.state == stateCancelled {
		entry.pending = false
		entry.err = conflict("session is cancelled")
		s.mu.Unlock()
		return domain.Reply{}, safeError(entry.err)
	}
	if sess.state == stateActionIssued {
		if sess.grant != nil && !now.Before(sess.grant.expiresAt) {
			sess.state = stateActive
			sess.grant = nil
			sess.resultRecorded = false
		} else {
			entry.pending = false
			entry.err = conflict("an action is already issued")
			s.mu.Unlock()
			return domain.Reply{}, safeError(entry.err)
		}
	}
	if sess.currentCancel != nil {
		sess.currentCancel()
	}
	sess.generation++
	generation := sess.generation
	workCtx, workCancel := context.WithCancel(ctx)
	sess.currentCancel = workCancel
	sess.state = stateProcessing
	sess.proposal = nil
	sess.grant = nil
	sess.resultRecorded = false
	apps := cloneApps(sess.apps)
	contacts := cloneContacts(sess.contacts)
	s.mu.Unlock()

	reply, started, runErr := s.runTurn(workCtx, req, apps, contacts)
	workCancel()
	if !started && ctx.Err() != nil {
		s.removeTurnReservation(sessionID, key, entry, generation)
		return domain.Reply{}, cancelled()
	}
	return s.completeTurn(sessionID, key, generation, req.Screen.Version, reply, runErr)
}

// Confirm atomically consumes one action proposal.
func (s *Service) Confirm(ctx context.Context, device domain.Device, sessionID string, req domain.ConfirmationRequest) (domain.ConfirmationResult, error) {
	if err := validation.RequestID(req.RequestID); err != nil {
		return domain.ConfirmationResult{}, err
	}
	hash := hashJSON(req)
	key := ledgerKey("confirmation", req.RequestID)
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupSessionsLocked(now)
	sess, err := s.sessionLocked(device.ID, sessionID)
	if err != nil {
		return domain.ConfirmationResult{}, err
	}
	if entry, ok := sess.ledger[key]; ok {
		sess.expiresAt = now.Add(s.cfg.SessionTTL)
		return replayConfirmation(entry, hash)
	}
	if len(sess.ledger) >= maxLedgerEntries {
		return domain.ConfirmationResult{}, resourceExhausted("session request ledger is full")
	}
	if ctx.Err() != nil {
		return domain.ConfirmationResult{}, cancelled()
	}
	entry := &ledgerEntry{hash: hash, pending: true}
	sess.ledger[key] = entry
	sess.expiresAt = now.Add(s.cfg.SessionTTL)
	finish := func(out domain.ConfirmationResult, finishErr error) (domain.ConfirmationResult, error) {
		entry.pending = false
		entry.confirmation = cloneConfirmation(out)
		entry.err = safeError(finishErr)
		return cloneConfirmation(out), entry.err
	}
	if !boundedText(req.ActionID, 1, 128) || req.Generation == 0 || !boundedText(req.ScreenVersion, 1, 128) || (req.Decision != domain.DecisionConfirm && req.Decision != domain.DecisionDecline) {
		return finish(domain.ConfirmationResult{}, invalid("invalid confirmation request"))
	}
	if sess.state == stateCancelled {
		return finish(domain.ConfirmationResult{}, conflict("session is cancelled"))
	}
	p := sess.proposal
	if sess.state != stateAwaitingConfirmation || p == nil {
		return finish(domain.ConfirmationResult{}, conflict("no action is awaiting confirmation"))
	}
	if !now.Before(p.expiresAt) {
		sess.proposal = nil
		sess.state = stateActive
		return finish(domain.ConfirmationResult{}, expired("action proposal expired"))
	}
	if req.ActionID != p.action.ActionID || req.Generation != p.generation || req.ScreenVersion != p.screenVersion || actionHash(p.action) != p.actionHash {
		return finish(domain.ConfirmationResult{}, conflict("confirmation does not match the action proposal"))
	}
	sess.proposal = nil
	if req.Decision == domain.DecisionDecline {
		sess.state = stateActive
		return finish(domain.ConfirmationResult{Status: "declined"}, nil)
	}
	grant := domain.ActionGrant{
		ActionID:   p.action.ActionID,
		Kind:       p.action.Kind,
		AppRef:     p.action.AppRef,
		AppLabel:   p.action.AppLabel,
		ContactRef: p.action.ContactRef,
		ExpiresAt:  now.Add(s.cfg.ConfirmationTTL),
	}
	sess.grant = &grantBinding{grant: grant, expiresAt: grant.ExpiresAt}
	sess.resultRecorded = false
	sess.state = stateActionIssued
	return finish(domain.ConfirmationResult{Status: "confirmed", Grant: cloneGrant(&grant)}, nil)
}

// ReportResult records one allowlisted observed result for an issued grant.
func (s *Service) ReportResult(ctx context.Context, device domain.Device, sessionID string, req domain.ActionResultRequest) error {
	if err := validation.RequestID(req.RequestID); err != nil {
		return err
	}
	hash := hashJSON(req)
	key := ledgerKey("result", req.RequestID)
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupSessionsLocked(now)
	sess, err := s.sessionLocked(device.ID, sessionID)
	if err != nil {
		return err
	}
	if entry, ok := sess.ledger[key]; ok {
		sess.expiresAt = now.Add(s.cfg.SessionTTL)
		return replayVoid(entry, hash)
	}
	if len(sess.ledger) >= maxLedgerEntries {
		return resourceExhausted("session request ledger is full")
	}
	if ctx.Err() != nil {
		return cancelled()
	}
	entry := &ledgerEntry{hash: hash, pending: true}
	sess.ledger[key] = entry
	sess.expiresAt = now.Add(s.cfg.SessionTTL)
	finish := func(finishErr error) error {
		entry.pending = false
		entry.err = safeError(finishErr)
		return entry.err
	}
	if !boundedText(req.ActionID, 1, 128) {
		return finish(invalid("invalid action result"))
	}
	if sess.state == stateCancelled {
		return finish(conflict("session is cancelled"))
	}
	if sess.grant == nil || sess.state != stateActionIssued || sess.grant.grant.ActionID != req.ActionID {
		return finish(conflict("result does not match an issued action"))
	}
	if !now.Before(sess.grant.expiresAt) {
		return finish(expired("action grant expired"))
	}
	if sess.resultRecorded {
		return finish(conflict("action result already recorded"))
	}
	if !statusAllowed(sess.grant.grant.Kind, req.Status) {
		return finish(invalid("result status does not match action kind"))
	}
	sess.resultRecorded = true
	if req.Status == "cancelled" {
		sess.generation++
		sess.state = stateCancelled
		if sess.currentCancel != nil {
			sess.currentCancel()
			sess.currentCancel = nil
		}
	} else {
		sess.state = stateActive
	}
	return finish(nil)
}

// Cancel makes a session terminal and invalidates in-flight generations.
func (s *Service) Cancel(ctx context.Context, device domain.Device, sessionID string, req domain.CancelRequest) error {
	if err := validation.RequestID(req.RequestID); err != nil {
		return err
	}
	hash := hashJSON(req)
	key := ledgerKey("cancel", req.RequestID)
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupSessionsLocked(now)
	sess, err := s.sessionLocked(device.ID, sessionID)
	if err != nil {
		return err
	}
	if entry, ok := sess.ledger[key]; ok {
		sess.expiresAt = now.Add(s.cfg.SessionTTL)
		return replayVoid(entry, hash)
	}
	if len(sess.ledger) >= maxLedgerEntries {
		return resourceExhausted("session request ledger is full")
	}
	if ctx.Err() != nil {
		return cancelled()
	}
	entry := &ledgerEntry{hash: hash, pending: false}
	sess.ledger[key] = entry
	sess.expiresAt = now.Add(s.cfg.SessionTTL)
	if sess.state == stateCancelled {
		entry.err = conflict("session is already cancelled")
		return entry.err
	}
	sess.generation++
	sess.state = stateCancelled
	sess.proposal = nil
	if sess.currentCancel != nil {
		sess.currentCancel()
		sess.currentCancel = nil
	}
	return nil
}

// Ready reports whether required dependencies are configured.
func (s *Service) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return cancelled()
	}
	if s.planner == nil || s.transcriber == nil || s.clock == nil {
		return &domain.AppError{Code: domain.CodeProviderUnavailable, Message: "provider configuration unavailable"}
	}
	return nil
}

func (s *Service) runTurn(ctx context.Context, req domain.TurnRequest, apps []domain.App, contacts []domain.Contact) (domain.Reply, bool, error) {
	base := req
	base.Image = nil
	base.Screen.Protected = false
	base.Screen.Sensitive = false
	if err := validation.Turn(base); err != nil {
		return domain.Reply{}, false, err
	}
	hasSensitiveNode := false
	for _, n := range req.Screen.Nodes {
		if n.Sensitive {
			hasSensitiveNode = true
			break
		}
	}
	if req.Screen.Protected || req.Screen.Sensitive || (req.Image != nil && hasSensitiveNode) {
		return refusal(req.Screen.Version, "Защищённый или чувствительный экран не обрабатывается."), false, nil
	}
	if req.Image != nil && (!req.Image.Consent || req.Image.ScreenVersion != req.Screen.Version) {
		return refusal(req.Screen.Version, "Для изображения нужно отдельное согласие на текущую версию экрана."), false, nil
	}
	if err := validation.Turn(req); err != nil {
		return domain.Reply{}, false, err
	}
	sanitized, err := policy.SanitizeSnapshot(req.Screen)
	if err != nil {
		return domain.Reply{}, false, &domain.AppError{Code: domain.CodeUnsafe, Message: "screen cannot be processed safely"}
	}
	decision, err := policy.Interpret(req.Text, sanitized, apps, contacts)
	if err != nil {
		return domain.Reply{}, false, err
	}
	if decision.Final {
		return decision.Reply, false, nil
	}
	if req.Image == nil && !hasUsableNode(sanitized) {
		if hasSensitiveNode {
			return refusal(req.Screen.Version, "На экране нет безопасных данных для обработки."), false, nil
		}
		return domain.Reply{Kind: domain.ReplyNeedImage, Text: "Нужно явное согласие на снимок текущего экрана.", ScreenVersion: req.Screen.Version}, false, nil
	}
	if ctx.Err() != nil {
		return domain.Reply{}, false, cancelled()
	}
	out, started, err := s.callPlanner(ctx, domain.PlanInput{Text: req.Text, Screen: sanitized, Image: cloneImage(req.Image), Apps: apps, Contacts: contacts})
	if err != nil {
		return domain.Reply{}, started, err
	}
	if err := policy.ValidateModelOutput(out, sanitized, apps, contacts); err != nil {
		return domain.Reply{}, true, err
	}
	if out.Reply.Kind == domain.ReplyActionProposal {
		return domain.Reply{}, true, &domain.AppError{Code: domain.CodeUnsafe, Message: "provider action lacks explicit deterministic authorization"}
	}
	return out.Reply, true, nil
}

func (s *Service) completeTurn(sessionID, key string, generation uint64, screenVersion string, reply domain.Reply, runErr error) (domain.Reply, error) {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sessionID]
	if sess == nil {
		return domain.Reply{}, notFound()
	}
	if !now.Before(sess.expiresAt) {
		sess.generation++
		if sess.currentCancel != nil {
			sess.currentCancel()
		}
		delete(s.sessions, sessionID)
		return domain.Reply{}, expired("session expired")
	}
	entry := sess.ledger[key]
	if entry == nil {
		return domain.Reply{}, cancelled()
	}
	if sess.generation != generation || sess.state != stateProcessing {
		runErr = conflict("turn result was superseded")
		reply = domain.Reply{}
	}
	if runErr != nil {
		if sess.generation == generation && sess.state == stateProcessing {
			sess.state = stateActive
			sess.currentCancel = nil
		}
		entry.pending = false
		entry.err = safeError(runErr)
		return domain.Reply{}, entry.err
	}
	reply.Generation = generation
	reply.ScreenVersion = screenVersion
	if reply.Kind == domain.ReplyActionProposal {
		if reply.Action == nil {
			entry.pending = false
			entry.err = internal("action reply is incomplete")
			sess.state = stateActive
			return domain.Reply{}, entry.err
		}
		action := *reply.Action
		action.ActionID = s.newID("act")
		reply.Action = &action
		sess.proposal = &proposalBinding{
			action:        action,
			actionHash:    actionHash(action),
			generation:    generation,
			screenVersion: screenVersion,
			expiresAt:     now.Add(s.cfg.ConfirmationTTL),
		}
		sess.state = stateAwaitingConfirmation
	} else {
		sess.state = stateActive
	}
	sess.currentCancel = nil
	entry.pending = false
	entry.reply = cloneReply(reply)
	return cloneReply(reply), nil
}

func (s *Service) completeTranscription(deviceID, key string, expectedLedger *deviceLedger, expectedEntry *ledgerEntry, out domain.Transcription, err error) (domain.Transcription, error) {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ledger := s.transcriptions[deviceID]
	if ledger == nil || ledger != expectedLedger || ledger.entries[key] != expectedEntry {
		return domain.Transcription{}, conflict("transcription result was superseded")
	}
	if !now.Before(ledger.expiresAt) {
		delete(s.transcriptions, deviceID)
		return domain.Transcription{}, expired("transcription request expired")
	}
	entry := ledger.entries[key]
	entry.pending = false
	entry.err = safeError(err)
	if entry.err == nil {
		entry.reply.Text = out.Text
	}
	if entry.err != nil {
		return domain.Transcription{}, entry.err
	}
	return out, nil
}

func (s *Service) callPlanner(ctx context.Context, in domain.PlanInput) (domain.PlanOutput, bool, error) {
	if s.planner == nil {
		return domain.PlanOutput{}, false, providerUnavailable()
	}
	deadline := s.clock.Now().Add(s.cfg.ProviderTimeout)
	timer := s.clock.After(s.cfg.ProviderTimeout)
	select {
	case s.providerSlots <- struct{}{}:
	case <-ctx.Done():
		return domain.PlanOutput{}, false, cancelled()
	case <-timer:
		return domain.PlanOutput{}, false, providerUnavailable()
	}
	if ctx.Err() != nil {
		<-s.providerSlots
		return domain.PlanOutput{}, false, cancelled()
	}
	if !s.clock.Now().Before(deadline) {
		<-s.providerSlots
		return domain.PlanOutput{}, false, providerUnavailable()
	}
	providerCtx, cancel := context.WithCancel(ctx)
	type result struct {
		out domain.PlanOutput
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		defer func() { <-s.providerSlots }()
		out, err := s.planner.Plan(providerCtx, in)
		resultCh <- result{out: out, err: err}
	}()
	select {
	case got := <-resultCh:
		cancel()
		if ctx.Err() != nil {
			return domain.PlanOutput{}, true, cancelled()
		}
		if !s.clock.Now().Before(deadline) {
			return domain.PlanOutput{}, true, providerUnavailable()
		}
		if got.err != nil {
			return domain.PlanOutput{}, true, providerUnavailable()
		}
		return got.out, true, nil
	case <-ctx.Done():
		cancel()
		return domain.PlanOutput{}, true, cancelled()
	case <-timer:
		cancel()
		return domain.PlanOutput{}, true, providerUnavailable()
	}
}

func (s *Service) callTranscriber(ctx context.Context, wav []byte) (string, bool, error) {
	if s.transcriber == nil {
		return "", false, providerUnavailable()
	}
	deadline := s.clock.Now().Add(s.cfg.ProviderTimeout)
	timer := s.clock.After(s.cfg.ProviderTimeout)
	select {
	case s.providerSlots <- struct{}{}:
	case <-ctx.Done():
		return "", false, cancelled()
	case <-timer:
		return "", false, providerUnavailable()
	}
	if ctx.Err() != nil {
		<-s.providerSlots
		return "", false, cancelled()
	}
	if !s.clock.Now().Before(deadline) {
		<-s.providerSlots
		return "", false, providerUnavailable()
	}
	providerCtx, cancel := context.WithCancel(ctx)
	type result struct {
		text string
		err  error
	}
	resultCh := make(chan result, 1)
	wavCopy := append([]byte(nil), wav...)
	go func() {
		defer func() { <-s.providerSlots }()
		text, err := s.transcriber.Transcribe(providerCtx, wavCopy)
		resultCh <- result{text: text, err: err}
	}()
	select {
	case got := <-resultCh:
		cancel()
		if ctx.Err() != nil {
			return "", true, cancelled()
		}
		if !s.clock.Now().Before(deadline) {
			return "", true, providerUnavailable()
		}
		if got.err != nil {
			return "", true, providerUnavailable()
		}
		return got.text, true, nil
	case <-ctx.Done():
		cancel()
		return "", true, cancelled()
	case <-timer:
		cancel()
		return "", true, providerUnavailable()
	}
}

func boundedConfig(cfg Config) Config {
	defaults := DefaultConfig()
	if cfg.SessionTTL <= 0 || cfg.SessionTTL > defaults.SessionTTL {
		cfg.SessionTTL = defaults.SessionTTL
	}
	if cfg.ConfirmationTTL <= 0 || cfg.ConfirmationTTL > defaults.ConfirmationTTL {
		cfg.ConfirmationTTL = defaults.ConfirmationTTL
	}
	if cfg.MaxSessions <= 0 || cfg.MaxSessions > defaults.MaxSessions {
		cfg.MaxSessions = defaults.MaxSessions
	}
	if cfg.MaxProviderCalls <= 0 || cfg.MaxProviderCalls > defaults.MaxProviderCalls {
		cfg.MaxProviderCalls = defaults.MaxProviderCalls
	}
	if cfg.ProviderTimeout <= 0 || cfg.ProviderTimeout > defaults.ProviderTimeout {
		cfg.ProviderTimeout = defaults.ProviderTimeout
	}
	return cfg
}

func validateSessionConfig(cfg domain.SessionConfig) error {
	if cfg.DeviceID == "" || len(cfg.DeviceID) > 128 || !utf8.ValidString(cfg.DeviceID) {
		return invalid("invalid device binding")
	}
	if len(cfg.Apps) > 128 || len(cfg.Contacts) > 32 {
		return resourceExhausted("too many configured targets")
	}
	if err := validateTargets(cfg.Apps, nil); err != nil {
		return err
	}
	return validateTargets(nil, cfg.Contacts)
}

func validateTargets(apps []domain.App, contacts []domain.Contact) error {
	seen := make(map[string]struct{}, len(apps)+len(contacts))
	check := func(ref, label string, aliases []string, contact bool) error {
		if !boundedText(ref, 1, 128) || !boundedText(label, 1, 128) || len(aliases) > 8 {
			return invalid("invalid configured target")
		}
		if _, ok := seen[ref]; ok {
			return invalid("duplicate configured target ref")
		}
		seen[ref] = struct{}{}
		for _, alias := range aliases {
			if !boundedText(alias, 1, 128) {
				return invalid("invalid configured target alias")
			}
		}
		if uriLikeTarget(ref) {
			return invalid("target refs must be opaque")
		}
		if contact {
			if resemblesPhoneNumber(ref) || containsPhoneNumber(ref) || containsPhoneNumber(label) || uriLikeTarget(label) {
				return invalid("contact metadata must not contain phone or execution targets")
			}
			for _, alias := range aliases {
				if containsPhoneNumber(alias) || uriLikeTarget(alias) {
					return invalid("contact metadata must not contain phone or execution targets")
				}
			}
		}
		return nil
	}
	for _, app := range apps {
		if err := check(app.Ref, app.Label, app.Aliases, false); err != nil {
			return err
		}
	}
	for _, contact := range contacts {
		if err := check(contact.Ref, contact.Label, contact.Aliases, true); err != nil {
			return err
		}
	}
	return nil
}

func resemblesPhoneNumber(ref string) bool {
	digits := 0
	for _, r := range ref {
		if unicode.IsDigit(r) {
			digits++
			continue
		}
		if !strings.ContainsRune("+()- .", r) {
			return false
		}
	}
	return digits >= 5
}

func containsPhoneNumber(value string) bool {
	digits := 0
	for _, r := range value {
		if unicode.IsDigit(r) {
			digits++
			if digits >= 7 {
				return true
			}
			continue
		}
		if strings.ContainsRune("+()- .", r) {
			continue
		}
		digits = 0
	}
	return false
}

func uriLikeTarget(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"tel:", "sms:", "smsto:", "intent:", "http://", "https://"} {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func (s *Service) sessionLocked(deviceID, sessionID string) (*sessionRecord, error) {
	sess := s.sessions[sessionID]
	if sess == nil || deviceID == "" || sess.deviceID != deviceID {
		return nil, notFound()
	}
	return sess, nil
}

func (s *Service) cleanupSessionsLocked(now time.Time) {
	for id, sess := range s.sessions {
		if !now.Before(sess.expiresAt) {
			sess.generation++
			if sess.currentCancel != nil {
				sess.currentCancel()
			}
			delete(s.sessions, id)
		}
	}
}

func (s *Service) cleanupTranscriptionsLocked(now time.Time) {
	for deviceID, ledger := range s.transcriptions {
		if !now.Before(ledger.expiresAt) {
			delete(s.transcriptions, deviceID)
		}
	}
}

func (s *Service) removeTurnReservation(sessionID, key string, expected *ledgerEntry, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sessionID]
	if sess == nil || sess.ledger[key] != expected {
		return
	}
	delete(sess.ledger, key)
	if sess.generation == generation && sess.state == stateProcessing {
		sess.state = stateActive
		sess.currentCancel = nil
	}
}

func (s *Service) removeTranscriptionReservation(deviceID, key string, expected *ledgerEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ledger := s.transcriptions[deviceID]; ledger != nil && ledger.entries[key] == expected {
		delete(ledger.entries, key)
	}
}

func replayReply(entry *ledgerEntry, hash [32]byte) (domain.Reply, error) {
	if entry.hash != hash {
		return domain.Reply{}, conflict("request id was reused with a different payload")
	}
	if entry.pending {
		return domain.Reply{}, inProgress()
	}
	return cloneReply(entry.reply), safeError(entry.err)
}

func replayConfirmation(entry *ledgerEntry, hash [32]byte) (domain.ConfirmationResult, error) {
	if entry.hash != hash {
		return domain.ConfirmationResult{}, conflict("request id was reused with a different payload")
	}
	if entry.pending {
		return domain.ConfirmationResult{}, inProgress()
	}
	return cloneConfirmation(entry.confirmation), safeError(entry.err)
}

func replayVoid(entry *ledgerEntry, hash [32]byte) error {
	if entry.hash != hash {
		return conflict("request id was reused with a different payload")
	}
	if entry.pending {
		return inProgress()
	}
	return safeError(entry.err)
}

func replayTranscription(entry *ledgerEntry, hash [32]byte) (domain.Transcription, error) {
	if entry.hash != hash {
		return domain.Transcription{}, conflict("request id was reused with a different payload")
	}
	if entry.pending {
		return domain.Transcription{}, inProgress()
	}
	if entry.err != nil {
		return domain.Transcription{}, safeError(entry.err)
	}
	return domain.Transcription{Text: entry.reply.Text}, nil
}

func hashJSON(v any) [32]byte {
	b, _ := json.Marshal(v)
	return sha256.Sum256(b)
}

func turnHash(req domain.TurnRequest) [32]byte {
	type canonicalImage struct {
		MediaType     string
		Consent       bool
		ScreenVersion string
		DecodedHash   [32]byte
		DecodeOK      bool
	}
	type canonicalTurn struct {
		RequestID string
		Text      string
		Screen    domain.ScreenSnapshot
		Image     *canonicalImage
	}
	c := canonicalTurn{RequestID: req.RequestID, Text: req.Text, Screen: req.Screen}
	if req.Image != nil {
		encoded := req.Image.DataBase64
		decodeOK := len(encoded) > 0 && len(encoded) <= 6990508 && !strings.ContainsAny(encoded, "\r\n\t ")
		decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
		decodeOK = decodeOK && err == nil && len(decoded) <= 5<<20
		if !decodeOK {
			decoded = []byte(req.Image.DataBase64)
		}
		c.Image = &canonicalImage{MediaType: req.Image.MediaType, Consent: req.Image.Consent, ScreenVersion: req.Image.ScreenVersion, DecodedHash: sha256.Sum256(decoded), DecodeOK: decodeOK}
	}
	return hashJSON(c)
}

func transcriptionHash(req domain.TranscriptionRequest) [32]byte {
	return hashJSON(struct {
		RequestID string
		WAVHash   [32]byte
	}{req.RequestID, sha256.Sum256(req.WAV)})
}

func actionHash(action domain.ActionProposal) [32]byte {
	return hashJSON(action)
}

func ledgerKey(endpoint, requestID string) string { return endpoint + "\x00" + requestID }

func (s *Service) newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%s-fallback-%d", prefix, s.idFallback.Add(1))
}

func cloneApps(in []domain.App) []domain.App {
	out := make([]domain.App, len(in))
	for i, app := range in {
		out[i] = app
		out[i].Aliases = append([]string(nil), app.Aliases...)
	}
	return out
}

func cloneContacts(in []domain.Contact) []domain.Contact {
	out := make([]domain.Contact, len(in))
	for i, contact := range in {
		out[i] = contact
		out[i].Aliases = append([]string(nil), contact.Aliases...)
	}
	return out
}

func cloneImage(in *domain.ImageInput) *domain.ImageInput {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneTurnRequest(in domain.TurnRequest) domain.TurnRequest {
	out := in
	out.Image = cloneImage(in.Image)
	out.Screen.Nodes = make([]domain.ScreenNode, len(in.Screen.Nodes))
	for i, node := range in.Screen.Nodes {
		out.Screen.Nodes[i] = node
		out.Screen.Nodes[i].Actions = append([]string(nil), node.Actions...)
	}
	return out
}

func cloneReply(in domain.Reply) domain.Reply {
	out := in
	out.HighlightNodeIDs = append([]string(nil), in.HighlightNodeIDs...)
	if in.Action != nil {
		action := *in.Action
		out.Action = &action
	}
	return out
}

func cloneGrant(in *domain.ActionGrant) *domain.ActionGrant {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneConfirmation(in domain.ConfirmationResult) domain.ConfirmationResult {
	out := in
	out.Grant = cloneGrant(in.Grant)
	return out
}

func hasUsableNode(screen domain.ScreenSnapshot) bool {
	for _, n := range screen.Nodes {
		if n.Visible {
			return true
		}
	}
	return false
}

func statusAllowed(kind domain.ActionKind, status string) bool {
	if status == "failed" || status == "cancelled" {
		return true
	}
	return (kind == domain.ActionOpenApp && status == "opened") || (kind == domain.ActionPrepareDialer && status == "dialer_presented")
}

func refusal(version, text string) domain.Reply {
	return domain.Reply{Kind: domain.ReplyRefuse, Text: text, ScreenVersion: version}
}

func boundedText(s string, minimum, maximum int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) >= minimum && utf8.RuneCountInString(s) <= maximum
}

func safeError(err error) error {
	if err == nil {
		return nil
	}
	var ae *domain.AppError
	if errors.As(err, &ae) {
		return &domain.AppError{Code: ae.Code, Message: ae.Message}
	}
	return internal("internal error")
}

func invalid(message string) error {
	return &domain.AppError{Code: domain.CodeInvalidArgument, Message: message}
}
func notFound() error {
	return &domain.AppError{Code: domain.CodeNotFound, Message: "session not found"}
}
func conflict(message string) error {
	return &domain.AppError{Code: domain.CodeConflict, Message: message}
}
func inProgress() error {
	return &domain.AppError{Code: domain.CodeInProgress, Message: "request is already in progress"}
}
func expired(message string) error {
	return &domain.AppError{Code: domain.CodeExpired, Message: message}
}
func resourceExhausted(message string) error {
	return &domain.AppError{Code: domain.CodeResourceExhausted, Message: message}
}
func providerUnavailable() error {
	return &domain.AppError{Code: domain.CodeProviderUnavailable, Message: "provider unavailable"}
}
func cancelled() error {
	return &domain.AppError{Code: domain.CodeConflict, Message: "request cancelled"}
}
func internal(message string) error {
	return &domain.AppError{Code: domain.CodeInternal, Message: message}
}
