package config

import (
	"errors"
	"testing"

	"github.com/100bench/OPORA/internal/infrastructure/openai"
)

func TestLoadDefaultsAndAllowsMissingProviderKey(t *testing.T) {
	env := map[string]string{"DEVICE_TOKENS_JSON": `{"device":"token"}`}
	cfg, err := Load(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != defaultListenAddress || cfg.OpenAIAPIKey != "" || cfg.PlannerModel != openai.PlannerModel || cfg.TranscriptionModel != openai.TranscriptionModel {
		t.Fatalf("cfg=%#v", cfg)
	}
}

func TestLoadRejectsUnsafeOrUnreviewedValues(t *testing.T) {
	cases := []map[string]string{
		{},
		{"DEVICE_TOKENS_JSON": `{}`, "LISTEN_ADDR": "not-an-address"},
		{"DEVICE_TOKENS_JSON": `{}`, "OPENAI_REASONING_MODEL": "unreviewed"},
		{"DEVICE_TOKENS_JSON": `{}`, "OPENAI_TRANSCRIPTION_MODEL": "unreviewed"},
	}
	for _, env := range cases {
		_, err := Load(func(key string) string { return env[key] })
		if !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("env=%#v err=%v", env, err)
		}
	}
}
