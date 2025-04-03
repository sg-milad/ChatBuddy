package main

import (
	"fmt"
	"os"
)

type Config struct {
	BotToken     string
	GeminiAPIKey string
}

const (
	envFileWarning = "No .env file found - using system environment variables"
	requiredErrFmt = "missing required environment variable: %s"
)

func LoadConfig() (*Config, error) {
	botToken, err := getRequiredEnv("TELEGRAM_BOT_TOKEN")
	if err != nil {
		return nil, fmt.Errorf("configuration error: %w", err)
	}

	geminiKey, err := getRequiredEnv("GEMINI_API_KEY")
	if err != nil {
		return nil, fmt.Errorf("configuration error: %w", err)
	}

	return &Config{
		BotToken:     botToken,
		GeminiAPIKey: geminiKey,
	}, nil
}

func getRequiredEnv(key string) (string, error) {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return "", fmt.Errorf(requiredErrFmt, key)
	}
	return value, nil
}
