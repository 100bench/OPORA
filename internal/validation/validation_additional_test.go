package validation

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/100bench/OPORA/internal/domain"
)

func TestValidationAdditionalTurnFields(t *testing.T) {
	badRequests := []domain.TurnRequest{
		{RequestID: "bad id", Text: "x", Screen: baseTurn().Screen},
		{RequestID: "r", Text: "", Screen: baseTurn().Screen},
		{RequestID: "r", Text: strings.Repeat("x", 4001), Screen: baseTurn().Screen},
	}
	for i, req := range badRequests {
		if err := Turn(req); err == nil {
			t.Errorf("bad request %d accepted", i)
		}
	}
	for _, id := range []string{"", "кириллица", strings.Repeat("x", 129)} {
		if err := RequestID(id); err == nil {
			t.Errorf("bad id %q accepted", id)
		}
	}

	cases := []func(*domain.TurnRequest){
		func(r *domain.TurnRequest) { r.Screen.Package = "" },
		func(r *domain.TurnRequest) { r.Screen.Version = strings.Repeat("v", 129) },
		func(r *domain.TurnRequest) { r.Screen.Width = 0 },
		func(r *domain.TurnRequest) { r.Screen.Height = 10001 },
		func(r *domain.TurnRequest) { r.Screen.Protected = true },
		func(r *domain.TurnRequest) { r.Screen.Nodes = []domain.ScreenNode{{ID: ""}} },
		func(r *domain.TurnRequest) { r.Screen.Nodes = []domain.ScreenNode{{ID: strings.Repeat("i", 129)}} },
		func(r *domain.TurnRequest) {
			r.Screen.Nodes = []domain.ScreenNode{{ID: "n", Bounds: domain.Bounds{Left: -1}}}
		},
		func(r *domain.TurnRequest) {
			r.Screen.Nodes = []domain.ScreenNode{{ID: "n", Bounds: domain.Bounds{Bottom: 2000}}}
		},
		func(r *domain.TurnRequest) {
			r.Screen.Nodes = []domain.ScreenNode{{ID: "n", Actions: make([]string, 17)}}
		},
		func(r *domain.TurnRequest) { r.Screen.Nodes = []domain.ScreenNode{{ID: "n", Actions: []string{"tap"}}} },
	}
	for i, mutate := range cases {
		req := baseTurn()
		mutate(&req)
		if err := Turn(req); err == nil {
			t.Errorf("bad screen %d accepted", i)
		}
	}
}

func TestValidationAdditionalImageFraming(t *testing.T) {
	jpegBytes := func() []byte {
		var b bytes.Buffer
		_ = jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
		return b.Bytes()
	}()
	valid := baseTurn()
	valid.Image = &domain.ImageInput{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(jpegBytes), Consent: true, ScreenVersion: "v"}
	if err := Turn(valid); err != nil {
		t.Fatal(err)
	}
	bad := []domain.ImageInput{
		{MediaType: "image/gif", DataBase64: "AAAA", Consent: true, ScreenVersion: "v"},
		{MediaType: "image/png", DataBase64: "", Consent: true, ScreenVersion: "v"},
		{MediaType: "image/png", DataBase64: "AA A=", Consent: true, ScreenVersion: "v"},
		{MediaType: "image/png", DataBase64: "!!!!", Consent: true, ScreenVersion: "v"},
		{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(append(append([]byte{}, jpegBytes...), 0xff, 0xd9)), Consent: true, ScreenVersion: "v"},
		{MediaType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(append(append([]byte{}, jpegBytes...), jpegBytes...)), Consent: true, ScreenVersion: "v"},
	}
	for i := range bad {
		req := baseTurn()
		req.Image = &bad[i]
		if err := Turn(req); err == nil {
			t.Errorf("bad image %d accepted", i)
		}
	}

	for _, raw := range [][]byte{
		{0xff, 0xd8, 0xff},
		{0xff, 0xd8, 0xff, 0xd8, 0xff, 0xd9},
		{0xff, 0xd8, 0xff, 0xda, 0, 1, 0xff, 0xd9},
		{0xff, 0xd8, 0xff, 0xe0, 0, 3, 0},
	} {
		if err := exactImageEnvelope("image/jpeg", raw); err == nil {
			t.Errorf("invalid JPEG accepted: %x", raw)
		}
	}
}

func TestValidationHugePNGRejectedBeforeDecode(t *testing.T) {
	var b bytes.Buffer
	// A syntactically complete PNG whose IHDR alone declares >4 MP. The test
	// ensures DecodeConfig/dimension rejection happens before pixel allocation.
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], 10000)
	binary.BigEndian.PutUint32(ihdr[4:8], 10000)
	ihdr[8], ihdr[9], ihdr[10], ihdr[11], ihdr[12] = 8, 6, 0, 0, 0
	writePNGChunk := func(name string, payload []byte) {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(payload))) // #nosec G115 -- payload is a fixed 13-byte test fixture.
		b.WriteString(name)
		b.Write(payload)
		crc := crc32.NewIEEE()
		_, _ = crc.Write([]byte(name))
		_, _ = crc.Write(payload)
		_ = binary.Write(&b, binary.BigEndian, crc.Sum32())
	}
	writePNGChunk("IHDR", ihdr)
	writePNGChunk("IEND", nil)
	req := baseTurn()
	req.Image = &domain.ImageInput{MediaType: "image/png", DataBase64: base64.StdEncoding.EncodeToString(b.Bytes()), Consent: true, ScreenVersion: "v"}
	if err := Turn(req); err == nil {
		t.Fatal("100 MP image accepted")
	}
}

func TestValidationAdditionalWAVChunks(t *testing.T) {
	missingData := wav(1, 16000, 16, 1)[:36]
	binary.LittleEndian.PutUint32(missingData[4:], uint32(len(missingData)-8)) // #nosec G115 -- tiny fixed fixture.
	missingFmt := append([]byte("RIFF\x08\x00\x00\x00WAVE"), []byte("data\x00\x00\x00\x00")...)
	nonPCM := wav(1, 16000, 16, 1)
	binary.LittleEndian.PutUint16(nonPCM[20:], 3)
	oddData := wav(1, 16000, 16, 1)
	binary.LittleEndian.PutUint32(oddData[40:], 3)
	oddData = oddData[:47]
	binary.LittleEndian.PutUint32(oddData[4:], uint32(len(oddData)-8)) // #nosec G115 -- tiny fixed fixture.
	for i, b := range [][]byte{missingData, missingFmt, nonPCM, oddData} {
		if err := WAV(b); err == nil {
			t.Errorf("bad WAV %d accepted", i)
		}
	}
}
