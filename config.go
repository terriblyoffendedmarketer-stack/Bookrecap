package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
)

type AppConfig struct {
	GoogleClientID     string `json:"google_client_id"`
	GoogleClientSecret string `json:"google_client_secret"`
	GoogleRedirectURI  string `json:"google_redirect_uri"`
	AnthropicAPIKey    string `json:"anthropic_api_key"`
	DriveFolderID      string `json:"drive_folder_id"`
	SecretKey          string `json:"-"`
	SecretBlockKey     string `json:"-"`
}

func loadConfig() AppConfig {
	return AppConfig{
		GoogleClientID:     configVal("google_client_id"),
		GoogleClientSecret: configVal("google_client_secret"),
		GoogleRedirectURI:  configVal("google_redirect_uri"),
		AnthropicAPIKey:    configVal("anthropic_api_key"),
		DriveFolderID:      configVal("drive_folder_id"),
		SecretKey:          getOrCreateSecret("secret_key"),
		SecretBlockKey:     getOrCreateSecret("secret_block_key"),
	}
}

// configVal reads from DB first, falls back to env var (for migration / local dev).
func configVal(key string) string {
	if v := getConfigVal(key); v != "" {
		return v
	}
	envMap := map[string]string{
		"google_client_id":     "GOOGLE_CLIENT_ID",
		"google_client_secret": "GOOGLE_CLIENT_SECRET",
		"google_redirect_uri":  "GOOGLE_REDIRECT_URI",
		"anthropic_api_key":    "ANTHROPIC_API_KEY",
	}
	if env, ok := envMap[key]; ok {
		return os.Getenv(env)
	}
	return ""
}

func getOrCreateSecret(key string) string {
	val := getConfigVal(key)
	if val != "" {
		return val
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("failed to generate secret: %v", err)
	}
	val = hex.EncodeToString(b)
	setConfigVal(key, val)
	return val
}

// configReady returns true if the minimum required keys are set.
func configReady() bool {
	cfg := loadConfig()
	return cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" && cfg.AnthropicAPIKey != ""
}
