package config

import (
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/100bench/OPORA/internal/infrastructure/openai"
)

const defaultListenAddress = "127.0.0.1:8080"

// ErrInvalidConfiguration is deliberately secret-free.
var ErrInvalidConfiguration = errors.New("runtime configuration is invalid")

// Config contains validated process configuration.
type Config struct {
	ListenAddress      string
	OpenAIAPIKey       string
	DeviceTokensJSON   string
	PlannerModel       string
	TranscriptionModel string
}

// Load reads and validates supported environment variables.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, ErrInvalidConfiguration
	}
	cfg := Config{
		ListenAddress:      strings.TrimSpace(getenv("LISTEN_ADDR")),
		OpenAIAPIKey:       strings.TrimSpace(getenv("OPENAI_API_KEY")),
		DeviceTokensJSON:   getenv("DEVICE_TOKENS_JSON"),
		PlannerModel:       strings.TrimSpace(getenv("OPENAI_REASONING_MODEL")),
		TranscriptionModel: strings.TrimSpace(getenv("OPENAI_TRANSCRIPTION_MODEL")),
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = defaultListenAddress
	}
	if cfg.PlannerModel == "" {
		cfg.PlannerModel = openai.PlannerModel
	}
	if cfg.TranscriptionModel == "" {
		cfg.TranscriptionModel = openai.TranscriptionModel
	}
	if !validListenAddress(cfg.ListenAddress) || cfg.DeviceTokensJSON == "" {
		return Config{}, ErrInvalidConfiguration
	}
	// These snapshots are part of the reviewed safety contract. Changing them
	// requires changing the constants, contract, tests, and documentation in the
	// same reviewed revision; an environment-only override is rejected.
	if cfg.PlannerModel != openai.PlannerModel || cfg.TranscriptionModel != openai.TranscriptionModel {
		return Config{}, ErrInvalidConfiguration
	}
	return cfg, nil
}

func validListenAddress(value string) bool {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || strings.ContainsAny(host, "\r\n") {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port >= 1 && port <= 65535
}
