package main

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	BotToken     string
	GeminiAPIKey string
}

func LoadConfig() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	return &Config{
		BotToken:     getEnv("TELEGRAM_BOT_TOKEN", ""),
		GeminiAPIKey: getEnv("GEMINI_API_KEY", ""),
	}
}

func getEnv(key, defaultVal string) string {
	if val, exists := os.LookupEnv(key); exists {
		return val
	}
	return defaultVal
}
