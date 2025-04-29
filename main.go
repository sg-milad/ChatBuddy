package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/generative-ai-go/genai"
	"github.com/robfig/cron/v3"
	"google.golang.org/api/option"
)

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
- /quiz [word] to create an instant quiz about a specific word
- /channelquiz [channelID] [word] to send a quiz to a specific channel
- /broadcastquiz [word] to send a quiz to ALL active channels
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

	bot, err = tgbotapi.NewBotAPI(cfg.BotToken)

	if err != nil {
		log.Panicf("failed to init bot API :%v", err)
	}

	bs := &BotService{
		api:            bot,
		gemini:         NewGeminiService(cfg.GeminiAPIKey),
		cron:           cron.New(cron.WithLocation(time.UTC)),
		activeChannels: make(map[int64]bool),
		activePolls:    make(map[string]*PollData),
	}

	bot.Debug = true

	// Schedule hourly quiz - run at minute 0 of every hour
	entryID, err := bs.cron.AddFunc("0 * * * *", bs.sendHourlyPoll)
	if err != nil {
		log.Printf("Error scheduling cron job: %v", err)
	}

	log.Printf("Successfully scheduled hourly quiz, entry ID: %v", entryID)

	// Start the scheduler
	bs.cron.Start()

	// Log the next run times for verification
	entries := bs.cron.Entries()
	for _, entry := range entries {
		log.Printf("Cron job ID %d scheduled, next run: %v", entry.ID, entry.Next)
	}

	log.Printf("authorized as @%s, scheduler started", bot.Self.UserName)
	return bs
}

// Run starts receiving updates with direct handling
func (bs *BotService) Run() {
	// Configure update parameters
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 60
	updateConfig.AllowedUpdates = []string{"message", "channel_post", "poll_answer"}

	// Main loop - don't use GetUpdatesChan
	for {
		// Get updates directly
		updates, err := bs.api.GetUpdates(updateConfig)

		if err != nil {
			log.Printf("Error getting updates: %v", err)

			// Handle conflict error by explicitly clearing the webhook
			if strings.Contains(err.Error(), "Conflict") {
				_, clearErr := bs.api.Request(tgbotapi.DeleteWebhookConfig{
					DropPendingUpdates: false,
				})

				if clearErr != nil {
					log.Printf("Failed to clear webhook: %v", clearErr)
				}
			}

			// Simple fixed delay
			time.Sleep(3 * time.Second)
			continue
		}

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
		helpMsg := fmt.Sprintf(botHelpMessage, bs.api.Self.UserName, bs.api.Self.UserName)
		// Add information about the quiz command
		helpMsg += "\n- /quiz [word] to create an instant quiz about a specific word"
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID, helpMsg))

	case "activate":
		bs.activeChannels[msg.Chat.ID] = true
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "✅ Quizzes activated in this chat."))

	case "deactivate":
		delete(bs.activeChannels, msg.Chat.ID)
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "❌ Quizzes deactivated in this chat."))

	case "quiz":
		bs.handleQuizCommand(msg)

	case "channelquiz":
		bs.handleChannelQuizCommand(msg)

	case "broadcastquiz":
		bs.handleBroadcastQuizCommand(msg)

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

func (bs *BotService) handleQuizCommand(msg *tgbotapi.Message) {
	// Extract the word parameter from the command
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID, "Please provide a word after /quiz command. Example: /quiz vocabulary"))
		return
	}

	// Let the user know we're generating a quiz
	statusMsg := tgbotapi.NewMessage(msg.Chat.ID, "Generating quiz for word: "+args+"...")
	bs.api.Send(statusMsg)

	// Generate and send the quiz
	bs.createAndSendQuiz(msg.Chat.ID, args)
}
func (bs *BotService) createAndSendQuiz(chatID int64, word string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Modified prompt to strongly emphasize no markdown and clean JSON
	prompt := fmt.Sprintf(`
	Create a multiple-choice English quiz about the word "%s". 
	This could involve its meaning, synonyms, antonyms, usage in a sentence, or related concepts.

	VERY IMPORTANT: Return ONLY raw JSON with NO markdown formatting, NO code blocks, NO backticks.
	Just the plain JSON object in this exact format:
	{"question":"...","choices":["opt1","opt2","opt3","opt4"],"answer_index":<0-3>}
	
	The response must start with { and end with } with no other text before or after.`, word)

	log.Printf("Generating quiz for word: %s", word)
	resp, err := bs.gemini.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("Quiz generation error: %v", err)
		bs.api.Send(tgbotapi.NewMessage(chatID, "Failed to generate quiz. Please try again later."))
		return
	}

	var quiz struct {
		Question    string   `json:"question"`
		Choices     []string `json:"choices"`
		AnswerIndex int      `json:"answer_index"`
	}

	// Extract raw response and clean it up
	raw := string(resp.Candidates[0].Content.Parts[0].(genai.Text))
	log.Printf("Raw quiz response: %s", raw)

	// Clean the raw response by extracting just the JSON part
	cleanedJSON := bs.extractJSON(raw)
	log.Printf("Cleaned JSON: %s", cleanedJSON)

	if err := json.Unmarshal([]byte(cleanedJSON), &quiz); err != nil {
		log.Printf("Quiz parse error: %v - raw: %s", err, raw)
		bs.api.Send(tgbotapi.NewMessage(chatID, "Failed to parse quiz. Please try again."))
		return
	}

	// Validate quiz data with detailed error logging
	if quiz.Question == "" {
		log.Printf("Quiz error: Empty question field")
		bs.api.Send(tgbotapi.NewMessage(chatID, "Quiz generation failed: Missing question. Please try again."))
		return
	}

	if len(quiz.Choices) != 4 {
		log.Printf("Quiz error: Expected 4 choices, got %d", len(quiz.Choices))
		bs.api.Send(tgbotapi.NewMessage(chatID, "Quiz generation failed: Wrong number of options. Please try again."))
		return
	}

	if quiz.AnswerIndex < 0 || quiz.AnswerIndex > 3 {
		log.Printf("Quiz error: Answer index %d out of range", quiz.AnswerIndex)
		bs.api.Send(tgbotapi.NewMessage(chatID, "Quiz generation failed: Invalid answer index. Please try again."))
		return
	}

	log.Printf("Sending quiz: %s", quiz.Question)

	pollCfg := tgbotapi.SendPollConfig{
		BaseChat:              tgbotapi.BaseChat{ChatID: chatID},
		Question:              quiz.Question,
		Options:               quiz.Choices,
		IsAnonymous:           false,
		AllowsMultipleAnswers: false,
		Type:                  "quiz",
	}

	sent, err := bs.api.Send(pollCfg)
	if err != nil {
		log.Printf("Failed to send poll to %d: %v", chatID, err)
		bs.api.Send(tgbotapi.NewMessage(chatID, "Failed to send quiz. Please try again later."))
		return
	}

	if sent.Poll != nil {
		log.Printf("Poll sent successfully, ID: %s", sent.Poll.ID)
		bs.activePolls[sent.Poll.ID] = &PollData{
			ChatID:        chatID,
			MessageID:     sent.MessageID,
			CorrectOption: quiz.AnswerIndex,
			Options:       quiz.Choices,
		}
	} else {
		log.Printf("Warning: Poll sent but no poll ID returned")
	}
}

// handleChannelQuizCommand processes the /channelquiz command
// Format: /channelquiz channelID word
// Example: /channelquiz -1001234567890 vocabulary
func (bs *BotService) handleChannelQuizCommand(msg *tgbotapi.Message) {
	args := strings.TrimSpace(msg.CommandArguments())
	parts := strings.SplitN(args, " ", 2)

	if len(parts) < 2 {
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			"Please provide both channel ID and word. Example: /channelquiz -1001234567890 vocabulary"))
		return
	}

	// Parse channel ID
	channelID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			"Invalid channel ID. Please use a numeric ID like -1001234567890"))
		return
	}

	word := parts[1]

	// Let the user know we're generating a quiz
	statusMsg := tgbotapi.NewMessage(msg.Chat.ID,
		fmt.Sprintf("Generating quiz about '%s' for channel %d...", word, channelID))
	bs.api.Send(statusMsg)

	// Generate the quiz
	quiz, err := bs.generateQuiz(word)
	if err != nil {
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf("Failed to generate quiz: %v", err)))
		return
	}

	// Send the quiz to the specified channel
	success := bs.sendQuizToChatID(channelID, quiz)

	// Confirm successful sending
	if success {
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf("Quiz about '%s' has been sent to channel %d ✅", word, channelID)))
	} else {
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf("Failed to send quiz to channel %d. Is the bot an admin there?", channelID)))
	}
}

// sendHourlyPoll creates and sends polls to activated channels
func (bs *BotService) sendHourlyPoll() {
	log.Printf("Hourly poll scheduler triggered. Active channels: %d", len(bs.activeChannels))

	if len(bs.activeChannels) == 0 {
		log.Printf("No active channels, skipping hourly poll")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prompt := `
	 Follow these response guidelines:
 1. DO NOT use markdown formatting (no asterisks for bold/italic),
	Create a multiple-choice English vocabulary question. Return JSON:
{"question":"...","choices":["opt1","opt2","opt3","opt4"],"answer_index":<0-3>}.`

	log.Printf("Generating quiz question...")
	resp, err := bs.gemini.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("Quiz generation error: %v", err)
		return
	}

	var quiz struct {
		Question    string   `json:"question"`
		Choices     []string `json:"choices"`
		AnswerIndex int      `json:"answer_index"`
	}

	raw := string(resp.Candidates[0].Content.Parts[0].(genai.Text))
	log.Printf("Raw quiz response: %s", raw)

	if err := json.Unmarshal([]byte(raw), &quiz); err != nil {
		log.Printf("Quiz parse error: %v - raw: %s", err, raw)
		return
	}

	// Validate quiz data
	if quiz.Question == "" || len(quiz.Choices) != 4 || quiz.AnswerIndex < 0 || quiz.AnswerIndex > 3 {
		log.Printf("Invalid quiz data: %+v", quiz)
		return
	}

	log.Printf("Sending quiz: %s", quiz.Question)

	for chatID := range bs.activeChannels {
		log.Printf("Sending poll to chat ID: %d", chatID)

		pollCfg := tgbotapi.SendPollConfig{
			BaseChat:              tgbotapi.BaseChat{ChatID: chatID},
			Question:              quiz.Question,
			Options:               quiz.Choices,
			IsAnonymous:           false,
			AllowsMultipleAnswers: false,
			Type:                  "quiz", // Make sure it's sent as a quiz
		}

		sent, err := bs.api.Send(pollCfg)
		if err != nil {
			log.Printf("Failed to send poll to %d: %v", chatID, err)
			continue
		}

		if sent.Poll != nil {
			log.Printf("Poll sent successfully, ID: %s", sent.Poll.ID)
			bs.activePolls[sent.Poll.ID] = &PollData{
				ChatID:        chatID,
				MessageID:     sent.MessageID,
				CorrectOption: quiz.AnswerIndex,
				Options:       quiz.Choices,
			}
		} else {
			log.Printf("Warning: Poll sent but no poll ID returned")
		}
	}
}

// handleBroadcastQuizCommand processes the /broadcastquiz command
// Format: /broadcastquiz word
// Example: /broadcastquiz vocabulary
func (bs *BotService) handleBroadcastQuizCommand(msg *tgbotapi.Message) {
	// Extract the word parameter from the command
	word := strings.TrimSpace(msg.CommandArguments())
	if word == "" {
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			"Please provide a word after /broadcastquiz command. Example: /broadcastquiz vocabulary"))
		return
	}

	// Let the user know we're starting the broadcast process
	statusMsg := tgbotapi.NewMessage(msg.Chat.ID,
		fmt.Sprintf("📢 Broadcasting quiz about '%s' to all active channels...", word))
	bs.api.Send(statusMsg)

	// Generate the quiz content once
	quizData, err := bs.generateQuiz(word)
	if err != nil {
		bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID,
			fmt.Sprintf("Failed to generate quiz: %v", err)))
		return
	}

	// Count active channels and successful broadcasts
	channelCount := len(bs.activeChannels)
	successCount := 0

	// Send the quiz to all active channels
	for chatID := range bs.activeChannels {
		success := bs.sendQuizToChatID(chatID, quizData)
		if success {
			successCount++
		}
	}

	// Report results to the user
	resultMsg := fmt.Sprintf("Quiz about '%s' broadcast complete! ✅\n%d/%d channels received the quiz successfully.",
		word, successCount, channelCount)
	bs.api.Send(tgbotapi.NewMessage(msg.Chat.ID, resultMsg))
}

// Define a struct to hold quiz data
type QuizData struct {
	Question    string
	Choices     []string
	AnswerIndex int
}

// generateQuiz creates a quiz about a specific word
func (bs *BotService) generateQuiz(word string) (*QuizData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Modified prompt to strongly emphasize no markdown and clean JSON
	prompt := fmt.Sprintf(`
	Create a multiple-choice English quiz about the word "%s". 
	This could involve its meaning, synonyms, antonyms, usage in a sentence, or related concepts.

	VERY IMPORTANT: Return ONLY raw JSON with NO markdown formatting, NO code blocks, NO backticks.
	Just the plain JSON object in this exact format:
	{"question":"...","choices":["opt1","opt2","opt3","opt4"],"answer_index":<0-3>}
	
	The response must start with { and end with } with no other text before or after.`, word)

	log.Printf("Generating quiz for word: %s", word)
	resp, err := bs.gemini.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("Quiz generation error: %v", err)
		return nil, fmt.Errorf("failed to generate quiz: %v", err)
	}

	var quiz QuizData

	// Extract raw response and clean it up
	raw := string(resp.Candidates[0].Content.Parts[0].(genai.Text))
	log.Printf("Raw quiz response: %s", raw)

	// Clean the raw response by extracting just the JSON part
	cleanedJSON := bs.extractJSON(raw)
	log.Printf("Cleaned JSON: %s", cleanedJSON)

	if err := json.Unmarshal([]byte(cleanedJSON), &quiz); err != nil {
		log.Printf("Quiz parse error: %v - raw: %s", err, raw)
		return nil, fmt.Errorf("failed to parse quiz: %v", err)
	}

	// Validate quiz data with detailed error logging
	if quiz.Question == "" {
		return nil, fmt.Errorf("quiz has empty question field")
	}

	if len(quiz.Choices) != 4 {
		return nil, fmt.Errorf("expected 4 choices, got %d", len(quiz.Choices))
	}

	if quiz.AnswerIndex < 0 || quiz.AnswerIndex > 3 {
		return nil, fmt.Errorf("answer index %d out of range", quiz.AnswerIndex)
	}

	return &quiz, nil
}

// extractJSON finds and extracts valid JSON from a string
// even if it's wrapped in markdown code blocks or has extra text
func (bs *BotService) extractJSON(text string) string {
	// Remove markdown code block markers if present
	text = strings.Replace(text, "```json", "", -1)
	text = strings.Replace(text, "```", "", -1)

	// Find the first opening curly brace
	startIndex := strings.Index(text, "{")
	if startIndex == -1 {
		return ""
	}

	// Find the last closing curly brace
	endIndex := strings.LastIndex(text, "}")
	if endIndex == -1 || endIndex < startIndex {
		return ""
	}

	// Extract just the JSON part
	return text[startIndex : endIndex+1]
}

// sendQuizToChatID sends a pre-generated quiz to a specific chat ID
func (bs *BotService) sendQuizToChatID(chatID int64, quiz *QuizData) bool {
	pollCfg := tgbotapi.SendPollConfig{
		BaseChat:              tgbotapi.BaseChat{ChatID: chatID},
		Question:              quiz.Question,
		Options:               quiz.Choices,
		IsAnonymous:           false,
		AllowsMultipleAnswers: false,
		Type:                  "quiz",
	}

	sent, err := bs.api.Send(pollCfg)
	if err != nil {
		log.Printf("Failed to send poll to %d: %v", chatID, err)
		return false
	}

	if sent.Poll != nil {
		log.Printf("Poll sent successfully to %d, ID: %s", chatID, sent.Poll.ID)
		bs.activePolls[sent.Poll.ID] = &PollData{
			ChatID:        chatID,
			MessageID:     sent.MessageID,
			CorrectOption: quiz.AnswerIndex,
			Options:       quiz.Choices,
		}
		return true
	} else {
		log.Printf("Warning: Poll sent but no poll ID returned for chat %d", chatID)
		return false
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
