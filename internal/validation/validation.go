// Package validation strictly parses bounded media and screen observations.
package validation

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/100bench/OPORA/internal/domain"
)

const (
	maxTreeBytes  = 256 << 10
	maxImageBytes = 5 << 20
	maxImagePixel = 4_000_000
	maxWAVBytes   = 1 << 20
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// Turn validates all domain-level limits after transport decoding.
func Turn(req domain.TurnRequest) error {
	if err := RequestID(req.RequestID); err != nil {
		return err
	}
	if err := sizedString(req.Text, 1, 4000, "turn text"); err != nil {
		return err
	}
	if err := snapshot(req.Screen); err != nil {
		return err
	}
	if req.Image != nil {
		if err := validateImage(*req.Image, req.Screen.Version); err != nil {
			return err
		}
	}
	return nil
}

// RequestID validates the common idempotency-key component.
func RequestID(id string) error {
	if len(id) == 0 || len(id) > 128 || !utf8.ValidString(id) || !requestIDPattern.MatchString(id) {
		return invalid("invalid request id")
	}
	return nil
}

func snapshot(s domain.ScreenSnapshot) error {
	if err := sizedString(s.Package, 1, 255, "screen package"); err != nil {
		return err
	}
	if err := sizedString(s.Version, 1, 128, "screen version"); err != nil {
		return err
	}
	if s.Width < 1 || s.Width > 10000 || s.Height < 1 || s.Height > 10000 {
		return invalid("invalid screen dimensions")
	}
	if s.Protected || s.Sensitive {
		return unsafe("protected or sensitive screen")
	}
	if len(s.Nodes) > 500 {
		return tooLarge("too many screen nodes")
	}
	seen := make(map[string]struct{}, len(s.Nodes))
	for _, n := range s.Nodes {
		if err := sizedString(n.ID, 1, 128, "node id"); err != nil {
			return err
		}
		if _, ok := seen[n.ID]; ok {
			return invalid("duplicate node id")
		}
		seen[n.ID] = struct{}{}
		if err := sizedString(n.Text, 0, 2048, "node text"); err != nil {
			return err
		}
		b := n.Bounds
		if b.Left < 0 || b.Top < 0 || b.Right < b.Left || b.Bottom < b.Top || b.Right > s.Width || b.Bottom > s.Height {
			return invalid("invalid node bounds")
		}
		if len(n.Actions) > 16 {
			return invalid("too many node actions")
		}
		for _, action := range n.Actions {
			switch action {
			case "click", "scroll", "set_text":
			default:
				return invalid("invalid node action")
			}
		}
	}
	b, err := json.Marshal(s.Nodes)
	if err != nil {
		return invalid("invalid screen tree")
	}
	if len(b) > maxTreeBytes {
		return tooLarge("screen tree too large")
	}
	return nil
}

func validateImage(in domain.ImageInput, screenVersion string) error {
	if in.MediaType != "image/png" && in.MediaType != "image/jpeg" {
		return unsupported("unsupported image media type")
	}
	if !in.Consent {
		return unsafe("image consent required")
	}
	if in.ScreenVersion == "" || in.ScreenVersion != screenVersion {
		return unsafe("image is not bound to the current screen")
	}
	if len(in.DataBase64) == 0 || len(in.DataBase64) > 6990508 {
		return tooLarge("invalid image size")
	}
	if strings.ContainsAny(in.DataBase64, "\r\n\t ") {
		return invalid("invalid base64 image")
	}
	b, err := base64.StdEncoding.Strict().DecodeString(in.DataBase64)
	if err != nil || len(b) == 0 {
		return invalid("invalid base64 image")
	}
	if len(b) > maxImageBytes {
		return tooLarge("image too large")
	}
	if err := exactImageEnvelope(in.MediaType, b); err != nil {
		return err
	}
	var config image.Config
	if in.MediaType == "image/jpeg" {
		config, err = jpeg.DecodeConfig(bytes.NewReader(b))
	} else {
		config, err = png.DecodeConfig(bytes.NewReader(b))
	}
	if err != nil {
		return invalid("invalid image")
	}
	w, h := int64(config.Width), int64(config.Height)
	if w <= 0 || h <= 0 || w > math.MaxInt32 || h > math.MaxInt32 || w*h > maxImagePixel {
		return tooLarge("image dimensions too large")
	}
	var img image.Image
	if in.MediaType == "image/jpeg" {
		img, err = jpeg.Decode(bytes.NewReader(b))
	} else {
		img, err = png.Decode(bytes.NewReader(b))
	}
	if err != nil {
		return invalid("invalid image")
	}
	bounds := img.Bounds()
	if bounds.Dx() != config.Width || bounds.Dy() != config.Height {
		return invalid("inconsistent image metadata")
	}
	return nil
}

func exactImageEnvelope(mediaType string, b []byte) error {
	if mediaType == "image/jpeg" {
		if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
			return unsupported("image media type mismatch")
		}
		return exactJPEG(b)
	}
	if len(b) < 8 || !bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return unsupported("image media type mismatch")
	}
	off, sawIHDR := 8, false
	for {
		if len(b)-off < 12 {
			return invalid("truncated PNG")
		}
		n := int(binary.BigEndian.Uint32(b[off : off+4]))
		if n < 0 || n > len(b)-off-12 {
			return invalid("truncated PNG chunk")
		}
		typ := b[off+4 : off+8]
		payloadEnd := off + 8 + n
		wantCRC := binary.BigEndian.Uint32(b[payloadEnd : payloadEnd+4])
		if crc32.ChecksumIEEE(b[off+4:payloadEnd]) != wantCRC {
			return invalid("invalid PNG checksum")
		}
		if !sawIHDR {
			if !bytes.Equal(typ, []byte("IHDR")) || n != 13 {
				return invalid("invalid PNG header")
			}
			sawIHDR = true
		}
		off = payloadEnd + 4
		if bytes.Equal(typ, []byte("IEND")) {
			if n != 0 || off != len(b) {
				return invalid("trailing PNG payload")
			}
			return nil
		}
	}
}

func exactJPEG(b []byte) error {
	pos := 2
	for pos < len(b) {
		if b[pos] != 0xff {
			return invalid("invalid JPEG marker")
		}
		for pos < len(b) && b[pos] == 0xff {
			pos++
		}
		if pos >= len(b) {
			return invalid("truncated JPEG marker")
		}
		marker := b[pos]
		pos++
		switch {
		case marker == 0xd9:
			if pos != len(b) {
				return invalid("trailing JPEG payload")
			}
			return nil
		case marker == 0xd8 || marker == 0x00 || marker == 0x01 || marker >= 0xd0 && marker <= 0xd7:
			return invalid("invalid JPEG marker order")
		}
		if len(b)-pos < 2 {
			return invalid("truncated JPEG segment")
		}
		n := int(binary.BigEndian.Uint16(b[pos : pos+2]))
		if n < 2 || n > len(b)-pos {
			return invalid("truncated JPEG segment")
		}
		pos += n
		if marker != 0xda {
			continue
		}
		for pos < len(b) {
			if b[pos] != 0xff {
				pos++
				continue
			}
			markerPos := pos
			for pos < len(b) && b[pos] == 0xff {
				pos++
			}
			if pos >= len(b) {
				return invalid("truncated JPEG scan")
			}
			next := b[pos]
			pos++
			switch {
			case next == 0x00, next >= 0xd0 && next <= 0xd7:
				continue
			case next == 0xd9:
				if pos != len(b) {
					return invalid("trailing JPEG payload")
				}
				return nil
			default:
				pos = markerPos
			}
			break
		}
	}
	return invalid("missing JPEG end marker")
}

// WAV strictly parses a bounded RIFF/WAVE PCM stream.
func WAV(b []byte) error {
	if len(b) > maxWAVBytes {
		return tooLarge("audio too large")
	}
	if len(b) < 12 || !bytes.Equal(b[:4], []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WAVE")) {
		return invalid("invalid WAV container")
	}
	if uint64(binary.LittleEndian.Uint32(b[4:8]))+8 != uint64(len(b)) {
		return invalid("invalid WAV length")
	}
	var (
		formatSeen, dataSeen bool
		channels, bits       uint16
		sampleRate, byteRate uint32
		blockAlign           uint16
		dataBytes            uint32
	)
	for off := 12; off < len(b); {
		if len(b)-off < 8 {
			return invalid("truncated WAV chunk")
		}
		name := string(b[off : off+4])
		rawSize := binary.LittleEndian.Uint32(b[off+4 : off+8])
		n := uint64(rawSize)
		start := uint64(off + 8)
		end := start + n
		if end > uint64(len(b)) {
			return invalid("truncated WAV chunk")
		}
		switch name {
		case "fmt ":
			if formatSeen || n != 16 {
				return invalid("invalid WAV format chunk")
			}
			formatSeen = true
			p := b[start:end]
			if binary.LittleEndian.Uint16(p[0:2]) != 1 {
				return unsupported("WAV must use PCM")
			}
			channels = binary.LittleEndian.Uint16(p[2:4])
			sampleRate = binary.LittleEndian.Uint32(p[4:8])
			byteRate = binary.LittleEndian.Uint32(p[8:12])
			blockAlign = binary.LittleEndian.Uint16(p[12:14])
			bits = binary.LittleEndian.Uint16(p[14:16])
		case "data":
			if dataSeen {
				return invalid("duplicate WAV data chunk")
			}
			dataSeen = true
			dataBytes = rawSize
		}
		next := end + n%2
		if next > uint64(len(b)) {
			return invalid("missing WAV chunk padding")
		}
		off = int(next) // #nosec G115 -- next is bounded by len(b), which is at most 1 MiB.
	}
	if !formatSeen || !dataSeen || channels != 1 || sampleRate != 16000 || bits != 16 {
		return unsupported("WAV must be mono 16 kHz 16-bit PCM")
	}
	wantAlign := uint16(channels * (bits / 8))
	wantRate := sampleRate * uint32(wantAlign)
	if blockAlign != wantAlign || byteRate != wantRate || dataBytes%uint32(wantAlign) != 0 {
		return invalid("inconsistent WAV format metadata")
	}
	if uint64(dataBytes) > uint64(byteRate)*30 {
		return tooLarge("audio duration exceeds 30 seconds")
	}
	return nil
}

func sizedString(s string, minimumRunes, maximumRunes int, field string) error {
	if !utf8.ValidString(s) {
		return invalid("invalid UTF-8 in " + field)
	}
	n := utf8.RuneCountInString(s)
	if n < minimumRunes || n > maximumRunes {
		return invalid("invalid " + field)
	}
	return nil
}

func invalid(message string) error {
	return &domain.AppError{Code: domain.CodeInvalidArgument, Message: message}
}

func tooLarge(message string) error {
	return &domain.AppError{Code: domain.CodePayloadTooLarge, Message: message}
}

func unsupported(message string) error {
	return &domain.AppError{Code: domain.CodeUnsupportedMedia, Message: message}
}

func unsafe(message string) error {
	return &domain.AppError{Code: domain.CodeUnsafe, Message: message}
}
