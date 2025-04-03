package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

type BotState struct {
	GeminiClient *genai.Client
	GeminiModel  *genai.GenerativeModel
}

func main() {
	cfg := LoadConfig()

	bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = true
	log.Printf("Authorized on account %s", bot.Self.UserName)

	geminiClient, geminiModel := initGemini(cfg.GeminiAPIKey)
	defer geminiClient.Close()

	state := &BotState{
		GeminiClient: geminiClient,
		GeminiModel:  geminiModel,
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message != nil {
			handleReplay(bot, update, state)
		}
	}
}

func initGemini(apiKey string) (*genai.Client, *genai.GenerativeModel) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Fatalf("Failed to create Gemini client: %v", err)
	}
	model := client.GenerativeModel("gemini-2.0-flash")
	return client, model
}

func handleReplay(bot *tgbotapi.BotAPI, update tgbotapi.Update, state *BotState) {
	msgText := update.Message.Text
	chatID := update.Message.Chat.ID
	botMention := "@" + bot.Self.UserName

	if update.Message.IsCommand() {
		handleCommands(bot, update)
		return
	}

	if !strings.Contains(strings.ToLower(msgText), strings.ToLower(botMention)) {
		return
	}

	var question string
	if update.Message.ReplyToMessage != nil {

		if strings.Contains(strings.ToLower(msgText), strings.ToLower(botMention)) {
			question += strings.Replace(msgText, botMention, "", -1)
		}

		question += update.Message.ReplyToMessage.Text
	} else {
		question = msgText
	}

	response := generateGeminiResponse(question, state)
	reply := tgbotapi.NewMessage(chatID, response)
	reply.ReplyToMessageID = update.Message.MessageID
	bot.Send(reply)
}

func handleCommands(bot *tgbotapi.BotAPI, update tgbotapi.Update) {
	botMention := "@" + bot.Self.UserName
	chatID := update.Message.Chat.ID
	reply := tgbotapi.NewMessage(chatID, "")

	if update.Message.Command() == "start" {
		reply.Text = "Hello! I’m ChatBuddy, your AI companion. Mention me with " + botMention + " to chat, or use /help for more info!"
		bot.Send(reply)
		return
	}

	if update.Message.Command() == "help" {
		reply.Text = "How to use me:\n- Mention me like " + botMention + " with a question or message.\n- I’ll reply with some AI magic!\n- Example: '" + botMention + " What’s the weather like?'"
		bot.Send(reply)
		return
	}
}

func generateGeminiResponse(message string, state *BotState) string {
	prompt := fmt.Sprintf(
		`You are a helpful Telegram bot that answers all types of questions thoroughly. The user asked: "%s"
	
		Follow these response guidelines:
		1. Address all parts of the question
		2. Start with a direct answer
		3. Provide context or examples when needed
		4. Use bullet points for complex explanations
		5. Keep technical answers precise but include layman terms
		
		Response language: Same as the user's message`,
		message,
	)

	ctx := context.Background()
	response, err := state.GeminiModel.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("Error generating response: %v", err)
		return "I can't process that right now, try again later!"
	}

	if len(response.Candidates) > 0 && len(response.Candidates[0].Content.Parts) > 0 {
		text, ok := response.Candidates[0].Content.Parts[0].(genai.Text)
		if ok {
			return string(text)
		}
	}

	return "I'm not sure how to respond to that."
}
