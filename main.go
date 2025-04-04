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

const (
	botHelpMessage = `How to use me:
- Mention me like %s with a question or message
- I'll reply with some AI magic!
- Example: '%s What's the weather like?'`

	responseErrorMsg = "I can't process that right now, try again later!"
	unknownCmdMsg    = "I'm not sure how to respond to that."
)

type GeminiService struct {
	client *genai.Client
	model  *genai.GenerativeModel
}

func NewGeminiService(apiKey string) *GeminiService {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		log.Fatalf("failed to initialize Gemini client: %v", err)
	}

	return &GeminiService{
		client: client,
		model:  client.GenerativeModel("gemini-2.0-flash"),
	}
}

func (gs *GeminiService) Close() {
	if err := gs.client.Close(); err != nil {
		log.Printf("error closing Gemini client: %v", err)
	}
}

type BotService struct {
	api        *tgbotapi.BotAPI
	gemini     *GeminiService
	botMention string
	id         int64
}

func NewBotService(cfg *Config) *BotService {
	bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Panicf("failed to initialize bot API: %v", err)
	}

	bot.Debug = true
	log.Printf("authorized as @%s", bot.Self.UserName)

	return &BotService{
		api:        bot,
		gemini:     NewGeminiService(cfg.GeminiAPIKey),
		botMention: "@" + bot.Self.UserName,
		id:         bot.Self.ID,
	}
}

func (bs *BotService) Run() {
	updates := bs.api.GetUpdatesChan(tgbotapi.NewUpdate(0))
	for update := range updates {
		bs.handleUpdate(update)
	}
}

func (bs *BotService) handleUpdate(update tgbotapi.Update) {
	if update.Message == nil {
		return
	}

	if update.Message.IsCommand() {
		bs.handleCommand(update.Message)
	} else if bs.isBotMentioned(update.Message.Text) {
		bs.handleQuery(update.Message)
	} else if update.Message.ReplyToMessage != nil && update.Message.ReplyToMessage.From.ID == bs.id {
		bs.handleQuery(update.Message)
	}
}

func (bs *BotService) handleCommand(msg *tgbotapi.Message) {
	response := tgbotapi.NewMessage(msg.Chat.ID, "")

	switch msg.Command() {
	case "start":
		response.Text = fmt.Sprintf("Hello! I'm ChatBuddy, your AI companion. Mention me with %s to chat, or use /help for more info!", bs.botMention)
	case "help":
		response.Text = fmt.Sprintf(botHelpMessage, bs.botMention, bs.botMention)
	default:
		response.Text = unknownCmdMsg
	}
	bs.sendResponse(response)
}

func (bs *BotService) handleQuery(msg *tgbotapi.Message) {
	question := bs.extractQuestion(msg)
	response := bs.generateResponse(question)

	reply := tgbotapi.NewMessage(msg.Chat.ID, response)
	reply.ReplyToMessageID = msg.MessageID
	bs.sendResponse(reply)
}

func (bs *BotService) isBotMentioned(text string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(bs.botMention))
}

func (bs *BotService) extractQuestion(msg *tgbotapi.Message) string {
	cleanText := strings.ReplaceAll(msg.Text, bs.botMention, "")

	if msg.ReplyToMessage != nil {
		return fmt.Sprintf("%s\n\n%s", cleanText, msg.ReplyToMessage.Text)
	}
	return cleanText
}

func (bs *BotService) generateResponse(query string) string {
	prompt := bs.buildPrompt(query)
	ctx := context.Background()

	resp, err := bs.gemini.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("gemini generation error: %v", err)
		return responseErrorMsg
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return unknownCmdMsg
	}

	if text, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
		return string(text)
	}
	return unknownCmdMsg
}

func (bs *BotService) buildPrompt(query string) string {
	return fmt.Sprintf(`You are a helpful Telegram bot that answers all types of questions thoroughly. The user asked: "%s"

    Follow these response guidelines:
    1. Address all parts of the question
    2. Start with a direct answer
    3. Provide context or examples when needed
    4. Use bullet points for complex explanations
    5. Keep technical answers precise but include layman terms

    Response language: Same as the user's message`, sanitizeInput(query))
}

func sanitizeInput(input string) string {
	return strings.ReplaceAll(input, "%", "%%")
}

func (bs *BotService) sendResponse(response tgbotapi.MessageConfig) {
	if _, err := bs.api.Send(response); err != nil {
		log.Printf("failed to send message: %v", err)
	}
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("Fatal configuration error: %v", err)
	}
	bot := NewBotService(cfg)
	defer bot.gemini.Close()
	bot.Run()
}
