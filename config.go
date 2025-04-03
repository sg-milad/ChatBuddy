package main

import (
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	BotToken     string
	GeminiAPIKey string
}

const (
	envFileWarning    = "No .env file found - using system environment variables"
	envFileLoadedMsg  = "Loaded .env file successfully"
	requiredErrFmt    = "missing required environment variable: %s"
	envFileLoadErrFmt = "WARNING: Error loading .env file: %v"
)

func LoadConfig() (*Config, error) {
	// Only load .env if file exists (local development)
	fmt.Println("TELEGRAM_BOT_TOKEN:", os.Getenv("TELEGRAM_BOT_TOKEN"))
	fmt.Println("GEMINI_API_KEY:", os.Getenv("GEMINI_API_KEY"))
	if isLocalEnv() {
		loadEnvFile()
	}

	// Validate required vars exist in environment
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

func isLocalEnv() bool {
	// Check for .env file existence
	_, err := os.Stat(".env")
	return !os.IsNotExist(err)
}

func loadEnvFile() {
	if err := godotenv.Load(); err != nil {
		log.Printf(envFileLoadErrFmt, err)
		return
	}
	log.Println(envFileLoadedMsg)
}

func getRequiredEnv(key string) (string, error) {
	if value := os.Getenv(key); value != "" {
		return value, nil
	}
	return "", fmt.Errorf(requiredErrFmt, key)
}
