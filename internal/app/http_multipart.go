package app

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"

	"github.com/100bench/OPORA/internal/domain"
	"github.com/100bench/OPORA/internal/service"
)

const (
	maxMultipartBody = 2 << 20
	maxAudioPart     = 1 << 20
)

var errPartTooLarge = errors.New("multipart part too large")

func transcribe(s service.Assistant) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "multipart/form-data" || params["boundary"] == "" {
			writeError(w, r, &domain.AppError{Code: domain.CodeUnsupportedMedia})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxMultipartBody)
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, r, invalid("invalid multipart body", err))
			return
		}
		var requestID string
		var audio []byte
		seenRequestID, seenAudio := false, false
		for {
			part, partErr := mr.NextPart()
			if errors.Is(partErr, io.EOF) {
				break
			}
			if partErr != nil {
				writeError(w, r, multipartReadError(partErr))
				return
			}
			name, filename := part.FormName(), part.FileName()
			switch name {
			case "request_id":
				if seenRequestID || filename != "" {
					_ = part.Close()
					writeError(w, r, invalid("invalid multipart fields", nil))
					return
				}
				b, readErr := readBoundedPart(part, 128)
				_ = part.Close()
				if readErr != nil || !utf8.Valid(b) {
					writeError(w, r, multipartReadError(readErr))
					return
				}
				requestID, seenRequestID = string(b), true
			case "audio":
				if seenAudio || filename == "" {
					_ = part.Close()
					writeError(w, r, invalid("invalid multipart fields", nil))
					return
				}
				audio, err = readBoundedPart(part, maxAudioPart)
				_ = part.Close()
				if err != nil {
					writeError(w, r, multipartReadError(err))
					return
				}
				seenAudio = true
			default:
				_ = part.Close()
				writeError(w, r, invalid("invalid multipart fields", nil))
				return
			}
		}
		if !seenRequestID || !seenAudio || requestID == "" {
			writeError(w, r, invalid("invalid multipart fields", nil))
			return
		}
		if s == nil {
			writeError(w, r, errors.New("assistant unavailable"))
			return
		}
		out, err := s.Transcribe(r.Context(), device(r), domain.TranscriptionRequest{RequestID: requestID, WAV: audio})
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func readBoundedPart(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errPartTooLarge
	}
	return b, nil
}

func multipartReadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return &domain.AppError{Code: domain.CodePayloadTooLarge, Cause: err}
	}
	return invalid("invalid multipart body", err)
}
