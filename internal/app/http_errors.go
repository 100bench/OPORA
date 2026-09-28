package app

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/100bench/OPORA/internal/domain"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type errorEnvelope struct {
	Error struct {
		Code      domain.ErrorCode `json:"code"`
		Message   string           `json:"message"`
		RequestID string           `json:"request_id,omitempty"`
	} `json:"error"`
}

func invalid(msg string, cause error) error {
	return &domain.AppError{Code: domain.CodeInvalidArgument, Message: msg, Cause: cause}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorCodeRecorder interface {
	setErrorCode(domain.ErrorCode)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	code := domain.CodeInternal
	var ae *domain.AppError
	if errors.As(err, &ae) {
		code = normalizeErrorCode(ae.Code)
	} else if errors.Is(err, domain.ErrNotImplemented) {
		code = domain.CodeNotImplemented
	}
	if recorder, ok := w.(errorCodeRecorder); ok {
		recorder.setErrorCode(code)
	}
	var e errorEnvelope
	e.Error.Code = code
	e.Error.Message = publicErrorMessage(code)
	if values := r.Header.Values("X-Request-ID"); len(values) == 1 && requestIDPattern.MatchString(values[0]) {
		e.Error.RequestID = values[0]
	}
	writeJSON(w, statusFor(code), e)
}

func normalizeErrorCode(code domain.ErrorCode) domain.ErrorCode {
	switch code {
	case domain.CodeInvalidArgument,
		domain.CodePayloadTooLarge,
		domain.CodeUnsupportedMedia,
		domain.CodeUnauthorized,
		domain.CodeNotFound,
		domain.CodeConflict,
		domain.CodeInProgress,
		domain.CodeExpired,
		domain.CodeUnsafe,
		domain.CodeResourceExhausted,
		domain.CodeProviderUnavailable,
		domain.CodeInternal,
		domain.CodeNotImplemented:
		return code
	default:
		return domain.CodeInternal
	}
}

func publicErrorMessage(code domain.ErrorCode) string {
	switch code {
	case domain.CodeInvalidArgument:
		return "invalid request"
	case domain.CodePayloadTooLarge:
		return "request body too large"
	case domain.CodeUnsupportedMedia:
		return "unsupported media type"
	case domain.CodeUnauthorized:
		return "authentication required"
	case domain.CodeNotFound:
		return "resource not found"
	case domain.CodeConflict:
		return "request conflicts with current state"
	case domain.CodeInProgress:
		return "request is already in progress"
	case domain.CodeExpired:
		return "request expired"
	case domain.CodeUnsafe:
		return "request cannot be completed safely"
	case domain.CodeResourceExhausted:
		return "resource limit reached"
	case domain.CodeProviderUnavailable:
		return "provider unavailable"
	case domain.CodeNotImplemented:
		return "not implemented"
	default:
		return "internal error"
	}
}

func statusFor(c domain.ErrorCode) int {
	switch c {
	case domain.CodeInvalidArgument:
		return http.StatusBadRequest
	case domain.CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case domain.CodeUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case domain.CodeUnauthorized:
		return http.StatusUnauthorized
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeConflict, domain.CodeInProgress:
		return http.StatusConflict
	case domain.CodeExpired:
		return http.StatusGone
	case domain.CodeUnsafe:
		return http.StatusUnprocessableEntity
	case domain.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case domain.CodeProviderUnavailable:
		return http.StatusServiceUnavailable
	case domain.CodeNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

type logWriter struct {
	http.ResponseWriter
	status    int
	errorCode domain.ErrorCode
}

func (w *logWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *logWriter) setErrorCode(code domain.ErrorCode) { w.errorCode = code }

func accessLog(l *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			lw := &logWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(lw, r)
			operation := chi.RouteContext(r.Context()).RoutePattern()
			if operation == "" {
				operation = "unmatched"
			}
			attrs := []any{
				"method", r.Method,
				"operation", operation,
				"status", lw.status,
				"latency_ms", time.Since(started).Milliseconds(),
			}
			if lw.errorCode != "" {
				attrs = append(attrs, "error_code", lw.errorCode)
			}
			l.InfoContext(r.Context(), "http_request", attrs...)
		})
	}
}
