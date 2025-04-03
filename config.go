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
	envFileWarning = "No .env file found - using system environment variables"
	requiredErrFmt = "missing required environment variable: %s"
)

func LoadConfig() (*Config, error) {
	// Check if the .env file exists for local development
	if _, err := os.Stat(".env"); err == nil {
		// .env file exists, so load it
		if err := godotenv.Load(); err != nil {
			log.Printf("WARNING: Error loading .env file: %v\n", err)
		} else {
			log.Println("Loaded .env file successfully")
		}
	} else if os.IsNotExist(err) {
		log.Println(envFileWarning)
	}

	// Retrieve the required environment variables
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
