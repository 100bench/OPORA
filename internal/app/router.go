package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/100bench/OPORA/internal/domain"
	"github.com/100bench/OPORA/internal/service"
)

// Authenticator resolves a bearer token to its server-owned device identity.
type Authenticator interface {
	Authenticate(string) (domain.Device, error)
}

// Dependencies contains the router's application ports and safe logger.
type Dependencies struct {
	Assistant service.Assistant
	Auth      Authenticator
	Logger    *slog.Logger
	Readiness func(context.Context) error
}

// NewRouter creates the complete public HTTP handler.
func NewRouter(d Dependencies) http.Handler {
	r := chi.NewRouter()
	if d.Logger != nil {
		r.Use(accessLog(d.Logger))
	}
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/ready", func(w http.ResponseWriter, r *http.Request) {
		var err error
		if d.Readiness != nil {
			err = d.Readiness(r.Context())
		} else if d.Assistant == nil {
			err = &domain.AppError{Code: domain.CodeProviderUnavailable}
		} else {
			err = d.Assistant.Ready(r.Context())
		}
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	r.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(d.Auth))
		r.Post("/sessions", createSession(d.Assistant))
		r.Post("/transcriptions", transcribe(d.Assistant))
		r.Route("/sessions/{session_id}", func(r chi.Router) {
			r.Post("/turns", turn(d.Assistant))
			r.Post("/confirmations", confirm(d.Assistant))
			r.Post("/results", result(d.Assistant))
			r.Post("/cancel", cancel(d.Assistant))
		})
	})
	return r
}

type ctxDeviceKey struct{}

func authMiddleware(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Values("Authorization"))
			if !ok || a == nil {
				writeError(w, r, &domain.AppError{Code: domain.CodeUnauthorized})
				return
			}
			device, err := a.Authenticate(token)
			if err != nil || device.ID == "" {
				writeError(w, r, &domain.AppError{Code: domain.CodeUnauthorized, Cause: err})
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxDeviceKey{}, device)))
		})
	}
}

func bearerToken(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	h := values[0]
	i := strings.IndexByte(h, ' ')
	if i <= 0 || !strings.EqualFold(h[:i], "Bearer") {
		return "", false
	}
	token := h[i+1:]
	if token == "" || token != strings.TrimSpace(token) || strings.ContainsAny(token, " \t\r\n,") {
		return "", false
	}
	return token, true
}

func device(r *http.Request) domain.Device {
	d, _ := r.Context().Value(ctxDeviceKey{}).(domain.Device)
	return d
}

type createSessionRequest struct {
	Apps     []domain.App     `json:"apps"`
	Contacts []domain.Contact `json:"contacts,omitempty"`
}

func createSession(s service.Assistant) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in createSessionRequest
		if err := decodeJSON(w, r, &in); err != nil {
			writeError(w, r, err)
			return
		}
		if s == nil {
			writeError(w, r, errors.New("assistant unavailable"))
			return
		}
		out, err := s.CreateSession(r.Context(), domain.SessionConfig{DeviceID: device(r).ID, Apps: in.Apps, Contacts: in.Contacts})
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, out)
	}
}

func turn(s service.Assistant) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in domain.TurnRequest
		if err := decodeJSON(w, r, &in); err != nil {
			writeError(w, r, err)
			return
		}
		if s == nil {
			writeError(w, r, errors.New("assistant unavailable"))
			return
		}
		out, err := s.Turn(r.Context(), device(r), chi.URLParam(r, "session_id"), in)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func confirm(s service.Assistant) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in domain.ConfirmationRequest
		if err := decodeJSON(w, r, &in); err != nil {
			writeError(w, r, err)
			return
		}
		if s == nil {
			writeError(w, r, errors.New("assistant unavailable"))
			return
		}
		out, err := s.Confirm(r.Context(), device(r), chi.URLParam(r, "session_id"), in)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func result(s service.Assistant) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in domain.ActionResultRequest
		if err := decodeJSON(w, r, &in); err != nil {
			writeError(w, r, err)
			return
		}
		if s == nil {
			writeError(w, r, errors.New("assistant unavailable"))
			return
		}
		if err := s.ReportResult(r.Context(), device(r), chi.URLParam(r, "session_id"), in); err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
	}
}

func cancel(s service.Assistant) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in domain.CancelRequest
		if err := decodeJSON(w, r, &in); err != nil {
			writeError(w, r, err)
			return
		}
		if s == nil {
			writeError(w, r, errors.New("assistant unavailable"))
			return
		}
		if err := s.Cancel(r.Context(), device(r), chi.URLParam(r, "session_id"), in); err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	}
}
