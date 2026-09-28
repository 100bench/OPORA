// Package domain defines transport-independent Opora values and errors.
package domain

import "errors"

// ErrNotImplemented marks an intentionally unavailable operation.
var ErrNotImplemented = errors.New("not implemented")

// ErrorCode is a stable public API error code.
type ErrorCode string

// Stable public API error codes.
const (
	CodeInvalidArgument     ErrorCode = "INVALID_ARGUMENT"
	CodePayloadTooLarge     ErrorCode = "PAYLOAD_TOO_LARGE"
	CodeUnsupportedMedia    ErrorCode = "UNSUPPORTED_MEDIA_TYPE"
	CodeUnauthorized        ErrorCode = "UNAUTHORIZED"
	CodeNotFound            ErrorCode = "NOT_FOUND"
	CodeConflict            ErrorCode = "CONFLICT"
	CodeInProgress          ErrorCode = "IN_PROGRESS"
	CodeExpired             ErrorCode = "EXPIRED"
	CodeUnsafe              ErrorCode = "UNSAFE_REQUEST"
	CodeResourceExhausted   ErrorCode = "RESOURCE_EXHAUSTED"
	CodeProviderUnavailable ErrorCode = "PROVIDER_UNAVAILABLE"
	CodeInternal            ErrorCode = "INTERNAL"
	CodeNotImplemented      ErrorCode = "NOT_IMPLEMENTED"
)

// AppError carries a safe public code and message plus an optional internal cause.
type AppError struct {
	Code    ErrorCode
	Message string
	Cause   error
}

func (e *AppError) Error() string { return string(e.Code) + ": " + e.Message }
func (e *AppError) Unwrap() error { return e.Cause }
