package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/generative-ai-go/genai"
	"github.com/robfig/cron/v3"
	"google.golang.org/api/option"
)

// Config holds environment configuration
// (Assumed to be defined in a separate file with LoadConfig)
// type Config struct {
// 	BotToken     string
// 	GeminiAPIKey string
// }

// PollData tracks each active poll's metadata
// and the chat context for channel posts
type PollData struct {
	ChatID        int64
	MessageID     int
	CorrectOption int
	Options       []string
}

// GeminiService wraps the Gemini client/model
type GeminiService struct {
	client *genai.Client
	model  *genai.GenerativeModel
}

// BotService is the main service struct
type BotService struct {
	api            *tgbotapi.BotAPI
	gemini         *GeminiService
	cron           *cron.Cron
	activeChannels map[int64]bool       // chatID → enabled/disabled
	activePolls    map[string]*PollData // pollID → PollData
}

const (
	// Usage/help message
	botHelpMessage = `How to use me:
- /activate to enable hourly English quizzes in this chat
- /deactivate to disable them
- Mention me like %s with a question or message
- I'll reply with some AI magic!
- Example: '%s What's the weather like?'
the creator❤️ @sg_milad`

	responseErrorMsg = "I can't process that right now, try again later!"
	unknownCmdMsg    = "I'm not sure how to respond to that."
)

// NewGeminiService initializes the Gemini client
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

// Close Gemini client
func (gs *GeminiService) Close() {
	if err := gs.client.Close(); err != nil {
		log.Printf("error closing Gemini client: %v", err)
	}
}

// NewBotService constructs BotService with retry logic
func NewBotService(cfg *Config) *BotService {
	// Initialize Telegram bot with retries
	var bot *tgbotapi.BotAPI
	var err error
	maxRetries := 5
	initialBackoff := 1 * time.Second
	maxBackoff := 30 * time.Second
	for attempt := 0; attempt < maxRetries; attempt++ {
		bot, err = tgbotapi.NewBotAPI(cfg.BotToken)
		if err == nil {
			break
		}
		if attempt < maxRetries-1 {
			backoff := initialBackoff * time.Duration(1<<attempt)
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			jitter := time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64()))
			log.Printf("failed to init bot API (attempt %d/%d): %v, retrying in %v",
				attempt+1, maxRetries, err, jitter)
			time.Sleep(jitter)
		}
	}
	if err != nil {
		log.Panicf("failed to init bot API after %d attempts: %v", maxRetries, err)
	}

	bs := &BotService{
		api:            bot,
		gemini:         NewGeminiService(cfg.GeminiAPIKey),
		cron:           cron.New(cron.WithLocation(time.UTC)),
		activeChannels: make(map[int64]bool),
		activePolls:    make(map[string]*PollData),
	}

	// Schedule hourly quiz at minute 0 of every hour
	bs.cron.AddFunc("@hourly", bs.sendHourlyPoll)
	bs.cron.Start()

	log.Printf("authorized as @%s, scheduler started", bot.Self.UserName)
	return bs
}

// Run starts receiving updates with robust conflict handling
func (bs *BotService) Run() {
	// Configure update parameters
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 60
	updateConfig.AllowedUpdates = []string{"message", "channel_post", "poll_answer"}

	// Base delay values for backoff
	baseDelay := 5 * time.Second
	maxDelay := 60 * time.Second
	currentDelay := baseDelay

	// Main loop - don't use GetUpdatesChan which can cause issues with conflict errors
	for {
		// Get updates directly instead of using the channel
		updates, err := bs.api.GetUpdates(updateConfig)

		if err != nil {
			// Check if it's a conflict error
			if strings.Contains(err.Error(), "Conflict") {
				log.Printf("Conflict detected: %v", err)
				log.Printf("Waiting %v before retrying...", currentDelay)

				// Clear the webhook explicitly to resolve conflict
				_, clearErr := bs.api.Request(tgbotapi.DeleteWebhookConfig{
					DropPendingUpdates: false,
				})

				if clearErr != nil {
					log.Printf("Failed to clear webhook: %v", clearErr)
				}

				// Exponential backoff with ceiling
				time.Sleep(currentDelay)
				currentDelay *= 2
				if currentDelay > maxDelay {
					currentDelay = maxDelay
				}
				continue
			}

			// For non-conflict errors
			log.Printf("Error getting updates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}

		// Reset delay on success
		currentDelay = baseDelay

		// Process updates
		for _, update := range updates {
			// Update offset for next polling
			updateConfig.Offset = update.UpdateID + 1

			// Handle poll answers
			if pa := update.PollAnswer; pa != nil {
				if pd, ok := bs.activePolls[pa.PollID]; ok {
					reveal := fmt.Sprintf(
						"✅ The correct answer is option %d: %s",
						pd.CorrectOption+1,
						pd.Options[pd.CorrectOption],
					)
					msg := tgbotapi.NewMessage(pd.ChatID, reveal)
					msg.ReplyToMessageID = pd.MessageID
					bs.api.Send(msg)
					delete(bs.activePolls, pa.PollID)
				}
				continue
			}

			// Handle channel posts
			if cp := update.ChannelPost; cp != nil {
				if cp.IsCommand() {
					bs.handleCommand(cp)
				}
				continue
			}

			// Handle private/group messages
			if msg := update.Message; msg != nil {
				if msg.IsCommand() {
					bs.handleCommand(msg)
				} else if bs.isBotMentioned(msg.Text) ||
					(msg.ReplyToMessage != nil && msg.ReplyToMessage.From.ID == bs.api.Self.ID) {
					bs.handleQuery(msg)
				}
			}
		}

		// Small pause to prevent CPU spinning on rapid polling
		time.Sleep(100 * time.Millisecond)
	}
}

// handleCommand processes bot commands for both messages and channel posts
func (bs *BotService) handleCommand(msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf("Hello! I'm ChatBuddy. Use /activate to turn on hourly quizzes, /deactivate to turn them off, or mention me to chat.")))

	case "help":
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf(botHelpMessage, bs.api.Self.UserName, bs.api.Self.UserName)))

	case "activate":
		bs.activeChannels[msg.Chat.ID] = true
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "✅ Quizzes activated in this chat."))

	case "deactivate":
		delete(bs.activeChannels, msg.Chat.ID)
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ Quizzes deactivated in this chat."))

	default:
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID, unknownCmdMsg))
	}
}

// handleQuery handles bot mentions and replies
func (bs *BotService) handleQuery(msg *tgbotapi.Message) {
	clean := strings.ReplaceAll(msg.Text, bs.api.Self.UserName, "")
	resp := bs.generateResponse(clean)
	reply := tgbotapi.NewMessage(msg.Chat.ID, resp)
	reply.ReplyToMessageID = msg.MessageID
	bs.sendChunks(reply)
}

// isBotMentioned checks for mention of the bot's username
func (bs *BotService) isBotMentioned(text string) bool {
	return strings.Contains(strings.ToLower(text),
		strings.ToLower(bs.api.Self.UserName))
}

// generateResponse queries Gemini for AI reply
func (bs *BotService) generateResponse(query string) string {
	prompt := fmt.Sprintf(`You are a helpful and witty Telegram bot. The user asked: "%s"

    Follow these response guidelines:
    1. Keep all responses brief and concise (2-3 sentences maximum)
    2. DO NOT use markdown formatting (no asterisks for bold/italic)
    3. Be conversational and friendly
    4. Focus only on the most essential information
    5. Learn from the user's instructions and feedback during this conversation and adapt your responses accordingly.
    Response language: Same as the user's message`, query)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := bs.gemini.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil || len(resp.Candidates) == 0 {
		log.Printf("Gemini error: %v", err)
		return responseErrorMsg
	}
	if txt, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
		return string(txt)
	}
	return unknownCmdMsg
}

// sendHourlyPoll creates and sends polls to activated channels
func (bs *BotService) sendHourlyPoll() {
	if len(bs.activeChannels) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prompt := `
    Follow these response guidelines:
 1. DO NOT use markdown formatting (no asterisks for bold/italic)
	Create a multiple-choice English vocabulary question. Return JSON:
{"question":"...","choices":["opt1","opt2","opt3","opt4"],"answer_index":<0-3>}.`
	resp, err := bs.gemini.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("Quiz gen error: %v", err)
		return
	}

	var quiz struct {
		Question    string   `json:"question"`
		Choices     []string `json:"choices"`
		AnswerIndex int      `json:"answer_index"`
	}
	raw := string(resp.Candidates[0].Content.Parts[0].(genai.Text))
	if err := json.Unmarshal([]byte(raw), &quiz); err != nil {
		log.Printf("Quiz parse error: %v - raw: %s", err, raw)
		return
	}

	for chatID := range bs.activeChannels {
		pollCfg := tgbotapi.SendPollConfig{
			BaseChat:              tgbotapi.BaseChat{ChatID: chatID},
			Question:              quiz.Question,
			Options:               quiz.Choices,
			IsAnonymous:           false,
			AllowsMultipleAnswers: false,
		}
		sent, err := bs.api.Send(pollCfg)
		if err != nil {
			log.Printf("failed to send poll to %d: %v", chatID, err)
			continue
		}
		if sent.Poll != nil {
			bs.activePolls[sent.Poll.ID] = &PollData{
				ChatID:        chatID,
				MessageID:     sent.MessageID,
				CorrectOption: quiz.AnswerIndex,
				Options:       quiz.Choices,
			}
		}
	}
}

// sendChunks splits long messages into 4096-char chunks
func (bs *BotService) sendChunks(msg tgbotapi.MessageConfig) {
	const max = 4096
	text := msg.Text
	for i := 0; i < len(text); i += max {
		end := i + max
		if end > len(text) {
			end = len(text)
		}
		chunk := tgbotapi.NewMessage(msg.ChatID, text[i:end])
		chunk.ReplyToMessageID = msg.ReplyToMessageID
		bs.api.Send(chunk)
	}
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}
	bot := NewBotService(cfg)
	defer bot.gemini.Close()
	bot.Run()
}
