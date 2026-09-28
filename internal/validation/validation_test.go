package validation

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/100bench/OPORA/internal/domain"
)

func baseTurn() domain.TurnRequest {
	return domain.TurnRequest{RequestID: "r", Text: "объясни", Screen: domain.ScreenSnapshot{Package: "p", Version: "v", Width: 1080, Height: 1920, Nodes: []domain.ScreenNode{}}}
}
func TestFR22_TreeBoundariesAndIntegrity(t *testing.T) {
	near := baseTurn()
	near.Screen.Nodes = make([]domain.ScreenNode, 121)
	for i := range near.Screen.Nodes {
		near.Screen.Nodes[i] = domain.ScreenNode{ID: fmt.Sprintf("near-%d", i), Text: strings.Repeat("a", 2048), Visible: true, Bounds: domain.Bounds{Right: 1, Bottom: 1}}
	}
	if err := Turn(near); err != nil {
		t.Fatalf("valid near-256KiB tree rejected: %v", err)
	}
	for _, n := range []int{500, 501} {
		r := baseTurn()
		r.Screen.Nodes = make([]domain.ScreenNode, n)
		for i := range r.Screen.Nodes {
			r.Screen.Nodes[i] = domain.ScreenNode{ID: fmt.Sprintf("n-%d", i), Visible: true, Bounds: domain.Bounds{Right: 1, Bottom: 1}}
		}
		err := Turn(r)
		if (n == 500) != (err == nil) {
			t.Errorf("nodes=%d err=%v", n, err)
		}
	}
	cases := []struct {
		name   string
		mutate func(*domain.TurnRequest)
	}{{"duplicate ids", func(r *domain.TurnRequest) { r.Screen.Nodes = []domain.ScreenNode{{ID: "x"}, {ID: "x"}} }}, {"invalid bounds", func(r *domain.TurnRequest) {
		r.Screen.Nodes = []domain.ScreenNode{{ID: "x", Bounds: domain.Bounds{Left: 10, Right: 1}}}
	}}, {"node text over 2048", func(r *domain.TurnRequest) {
		r.Screen.Nodes = []domain.ScreenNode{{ID: "x", Text: strings.Repeat("я", 2049)}}
	}}, {"tree over 256KiB", func(r *domain.TurnRequest) {
		r.Screen.Nodes = make([]domain.ScreenNode, 122)
		for i := range r.Screen.Nodes {
			r.Screen.Nodes[i] = domain.ScreenNode{ID: fmt.Sprintf("large-%d", i), Text: strings.Repeat("a", 2048)}
		}
	}}, {"sensitive screen", func(r *domain.TurnRequest) { r.Screen.Sensitive = true }}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := baseTurn()
			tc.mutate(&r)
			if err := Turn(r); err == nil {
				t.Fatal("invalid tree accepted")
			}
		})
	}
}

func TestFR23_ImageDecodeTypeSizeAndConsent(t *testing.T) {
	encode := func(format string, w, h int) string {
		var b bytes.Buffer
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		if format == "image/png" {
			_ = png.Encode(&b, img)
		} else {
			_ = jpeg.Encode(&b, img, nil)
		}
		return base64.StdEncoding.EncodeToString(b.Bytes())
	}
	valid := baseTurn()
	valid.Image = &domain.ImageInput{MediaType: "image/png", DataBase64: encode("image/png", 2000, 2000), Consent: true, ScreenVersion: "v"}
	if err := Turn(valid); err != nil {
		t.Fatalf("4MP PNG rejected: %v", err)
	}
	jpegReq := baseTurn()
	jpegReq.Image = &domain.ImageInput{MediaType: "image/jpeg", DataBase64: encode("image/jpeg", 1, 1), Consent: true, ScreenVersion: "v"}
	if err := Turn(jpegReq); err != nil {
		t.Fatalf("valid JPEG rejected: %v", err)
	}
	basePNG, _ := base64.StdEncoding.DecodeString(encode("image/png", 1, 1))
	exact := padPNG(t, basePNG, 5<<20)
	exactReq := baseTurn()
	exactReq.Image = &domain.ImageInput{MediaType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(exact), Consent: true, ScreenVersion: "v"}
	if err := Turn(exactReq); err != nil {
		t.Fatalf("exact 5MiB PNG rejected: %v", err)
	}
	for _, tc := range []struct {
		name, typ, data, version string
		consent                  bool
	}{{"over 4MP", "image/png", encode("image/png", 2001, 2000), "v", true}, {"mismatch", "image/jpeg", encode("image/png", 1, 1), "v", true}, {"truncated", "image/png", base64.StdEncoding.EncodeToString([]byte("\x89PNG")), "v", true}, {"polyglot", "image/png", base64.StdEncoding.EncodeToString(append([]byte("junk"), []byte("\x89PNG")...)), "v", true}, {"valid image trailing payload", "image/png", base64.StdEncoding.EncodeToString(append(basePNG, []byte("junk")...)), "v", true}, {"over 5MiB", "image/png", base64.StdEncoding.EncodeToString(append(exact, 0)), "v", true}, {"no consent", "image/png", encode("image/png", 1, 1), "v", false}, {"stale binding", "image/png", encode("image/png", 1, 1), "old", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := baseTurn()
			r.Image = &domain.ImageInput{MediaType: tc.typ, DataBase64: tc.data, Consent: tc.consent, ScreenVersion: tc.version}
			if err := Turn(r); err == nil {
				t.Fatal("invalid image accepted")
			}
		})
	}
}

func padPNG(t *testing.T, b []byte, target int) []byte {
	t.Helper()
	if len(b)+12 > target {
		t.Fatal("base PNG too large")
	}
	at := len(b) - 12
	n := target - len(b) - 12
	chunk := make([]byte, 12+n)
	binary.BigEndian.PutUint32(chunk, uint32(n)) // #nosec G115 -- test fixture n is bounded by 5 MiB.
	copy(chunk[4:8], "ruSt")
	crc := crc32.NewIEEE()
	_, _ = crc.Write(chunk[4 : 8+n])
	binary.BigEndian.PutUint32(chunk[8+n:], crc.Sum32())
	out := make([]byte, 0, target)
	out = append(out, b[:at]...)
	out = append(out, chunk...)
	out = append(out, b[at:]...)
	return out
}

func wav(channels, sampleRate, bits int, seconds float64) []byte {
	dataLen := int(float64(sampleRate*channels*(bits/8)) * seconds)
	b := make([]byte, 44+dataLen)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8)) // #nosec G115 -- fixture is bounded by 1 MiB.
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], uint16(channels))                     // #nosec G115 -- controlled fixture parameters.
	binary.LittleEndian.PutUint32(b[24:], uint32(sampleRate))                   // #nosec G115 -- controlled fixture parameters.
	binary.LittleEndian.PutUint32(b[28:], uint32(sampleRate*channels*(bits/8))) // #nosec G115 -- controlled fixture parameters.
	binary.LittleEndian.PutUint16(b[32:], uint16(channels*(bits/8)))            // #nosec G115 -- controlled fixture parameters.
	binary.LittleEndian.PutUint16(b[34:], uint16(bits))                         // #nosec G115 -- controlled fixture parameters.
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(dataLen)) // #nosec G115 -- fixture is bounded by 1 MiB.
	return b
}
func TestFR24_WAVStructureAndBoundaries(t *testing.T) {
	if err := WAV(wav(1, 16000, 16, 30)); err != nil {
		t.Fatalf("valid boundary rejected: %v", err)
	}
	withUnknown := wav(1, 16000, 16, 1)
	unknown := []byte{'L', 'I', 'S', 'T', 1, 0, 0, 0, 'x', 0}
	withUnknown = append(append(append([]byte{}, withUnknown[:36]...), unknown...), withUnknown[36:]...)
	binary.LittleEndian.PutUint32(withUnknown[4:], uint32(len(withUnknown)-8)) // #nosec G115 -- fixture is bounded by 1 MiB.
	if err := WAV(withUnknown); err != nil {
		t.Fatalf("padded unknown chunk rejected: %v", err)
	}
	declared := wav(1, 16000, 16, 1)
	binary.LittleEndian.PutUint32(declared[40:], uint32(len(declared))) // #nosec G115 -- fixture is bounded by 1 MiB.
	duplicate := append(wav(1, 16000, 16, 1), []byte("data\x00\x00\x00\x00")...)
	badRate := wav(1, 16000, 16, 1)
	binary.LittleEndian.PutUint32(badRate[28:], 0)
	badAlign := wav(1, 16000, 16, 1)
	binary.LittleEndian.PutUint16(badAlign[32:], 0)
	for _, b := range [][]byte{wav(2, 16000, 16, 1), wav(1, 8000, 16, 1), wav(1, 16000, 8, 1), wav(1, 16000, 16, 30.1), []byte("RIFFbad"), append(wav(1, 16000, 16, 1), make([]byte, 1<<20)...), declared, duplicate, badRate, badAlign} {
		if err := WAV(b); err == nil {
			t.Error("invalid WAV accepted")
		}
	}
}
