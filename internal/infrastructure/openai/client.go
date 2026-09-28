package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/100bench/OPORA/internal/domain"
)

const (
	// PlannerModel is the reviewed reasoning/vision snapshot.
	PlannerModel = "gpt-5.4-mini-2026-03-17"
	// TranscriptionModel is the reviewed speech-to-text snapshot.
	TranscriptionModel = "gpt-4o-mini-transcribe-2025-12-15"
	// DefaultEndpoint is the official OpenAI v1 API endpoint.
	DefaultEndpoint = "https://api.openai.com/v1"

	maxProviderTimeout = 20 * time.Second
	maxProviderBody    = 1 << 20
	maxStructuredText  = 64 << 10
)

// Config contains the in-memory OpenAI adapter configuration.
type Config struct {
	Endpoint           string
	APIKey             string
	PlannerModel       string
	TranscriptionModel string
	HTTPClient         *http.Client
	Timeout            time.Duration
}

// Client implements the service planner and transcriber ports.
type Client struct {
	endpoint           string
	apiKey             string
	plannerModel       string
	transcriptionModel string
	httpClient         *http.Client
	timeout            time.Duration
}

// New constructs a client with the reviewed model snapshots.
func New(endpoint, apiKey string, hc *http.Client) *Client {
	return NewWithConfig(Config{
		Endpoint:           endpoint,
		APIKey:             apiKey,
		PlannerModel:       PlannerModel,
		TranscriptionModel: TranscriptionModel,
		HTTPClient:         hc,
		Timeout:            maxProviderTimeout,
	})
}

// NewWithConfig constructs a client while enforcing the 20-second hard cap.
func NewWithConfig(cfg Config) *Client {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.PlannerModel == "" {
		cfg.PlannerModel = PlannerModel
	}
	if cfg.TranscriptionModel == "" {
		cfg.TranscriptionModel = TranscriptionModel
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	httpClient := *cfg.HTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if cfg.Timeout <= 0 || cfg.Timeout > maxProviderTimeout {
		cfg.Timeout = maxProviderTimeout
	}
	return &Client{
		endpoint:           strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:             cfg.APIKey,
		plannerModel:       cfg.PlannerModel,
		transcriptionModel: cfg.TranscriptionModel,
		httpClient:         &httpClient,
		timeout:            cfg.Timeout,
	}
}

// Ready verifies local configuration. It deliberately makes no network call
// and does not claim that the credential or upstream service is available.
func (c *Client) Ready() error {
	if c == nil || strings.TrimSpace(c.apiKey) == "" || c.plannerModel == "" || c.transcriptionModel == "" {
		return providerError(nil)
	}
	if _, err := c.endpointURL(""); err != nil {
		return providerError(err)
	}
	return nil
}

// Plan calls Responses and strictly parses one structured assistant message.
func (c *Client) Plan(ctx context.Context, in domain.PlanInput) (domain.PlanOutput, error) {
	if err := c.Ready(); err != nil {
		return domain.PlanOutput{}, err
	}
	input, err := responseInput(in)
	if err != nil {
		return domain.PlanOutput{}, providerError(err)
	}
	payload := map[string]any{
		"model":     c.plannerModel,
		"store":     false,
		"reasoning": map[string]string{"effort": "none"},
		"input":     input,
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   "opora_reply",
				"strict": true,
				"schema": responseSchema(),
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return domain.PlanOutput{}, providerError(err)
	}
	responseBody, err := c.do(ctx, http.MethodPost, "/responses", "application/json", body)
	if err != nil {
		return domain.PlanOutput{}, err
	}
	text, err := parseResponseOutput(responseBody)
	if err != nil {
		return domain.PlanOutput{}, providerError(err)
	}
	out, err := parseStructuredReply([]byte(text), in)
	if err != nil {
		return domain.PlanOutput{}, providerError(err)
	}
	return out, nil
}

// Transcribe sends an in-memory WAV to the transcription endpoint.
func (c *Client) Transcribe(ctx context.Context, wav []byte) (string, error) {
	if err := c.Ready(); err != nil {
		return "", err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="audio.wav"`)
	header.Set("Content-Type", "audio/wav")
	part, err := w.CreatePart(header)
	if err != nil {
		return "", providerError(err)
	}
	if _, err := part.Write(wav); err != nil {
		return "", providerError(err)
	}
	if err := w.WriteField("model", c.transcriptionModel); err != nil {
		return "", providerError(err)
	}
	if err := w.WriteField("response_format", "json"); err != nil {
		return "", providerError(err)
	}
	if err := w.WriteField("language", "ru"); err != nil {
		return "", providerError(err)
	}
	if err := w.Close(); err != nil {
		return "", providerError(err)
	}
	responseBody, err := c.do(ctx, http.MethodPost, "/audio/transcriptions", w.FormDataContentType(), body.Bytes())
	if err != nil {
		return "", err
	}
	if !utf8.Valid(responseBody) || rejectDuplicateJSONKeys(responseBody) != nil {
		return "", providerError(errors.New("invalid transcription response"))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(responseBody, &fields); err != nil || fields == nil {
		return "", providerError(errors.New("invalid transcription response"))
	}
	rawText, ok := fields["text"]
	if !ok {
		return "", providerError(errors.New("invalid transcription response"))
	}
	var text string
	if err := json.Unmarshal(rawText, &text); err != nil || utf8.RuneCountInString(text) > 4000 {
		return "", providerError(errors.New("transcription response too long"))
	}
	return text, nil
}

func (c *Client) do(ctx context.Context, method, suffix, contentType string, body []byte) ([]byte, error) {
	endpoint, err := c.endpointURL(suffix)
	if err != nil {
		return nil, providerError(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, providerError(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if callCtx.Err() != nil {
			return nil, providerError(callCtx.Err())
		}
		return nil, providerError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		return nil, providerError(nil)
	}
	responseBody, err := readBounded(resp.Body, maxProviderBody)
	if err != nil {
		return nil, providerError(err)
	}
	return responseBody, nil
}

func (c *Client) endpointURL(suffix string) (string, error) {
	u, err := url.Parse(c.endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid provider endpoint")
	}
	u.Path = strings.TrimRight(u.Path, "/") + suffix
	u.RawPath = ""
	return u.String(), nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("provider response too large")
	}
	return b, nil
}

func providerError(cause error) error {
	return &domain.AppError{Code: domain.CodeProviderUnavailable, Message: "provider unavailable", Cause: cause}
}
