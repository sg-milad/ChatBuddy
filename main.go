package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/generative-ai-go/genai"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/api/option"
)

const (
	botHelpMessage = `How to use me:
- Mention me like %s with a question or message
- I'll reply with some AI magic!
- Use /summary to get a summary of recent messages (up to 200)
- Example: '%s What's the weather like?' 
the creator❤️ @sg_milad`

	responseErrorMsg    = "I can't process that right now, try again later!"
	unknownCmdMsg       = "I'm not sure how to respond to that."
	fetchingMessagesMsg = "Fetching recent messages for summary... This may take a moment."
	maxMessagesToFetch  = 200
)

// Message represents a chat message stored in MongoDB
type Message struct {
	ChatID        int64     `bson:"chat_id"`
	MessageID     int       `bson:"message_id"`
	FromUsername  string    `bson:"from_username"`
	FromFirstName string    `bson:"from_first_name"`
	FromLastName  string    `bson:"from_last_name"`
	Text          string    `bson:"text"`
	Timestamp     time.Time `bson:"timestamp"`
}

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
	db         *mongo.Database
}

func NewBotService(cfg *Config) *BotService {
	bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Panicf("failed to initialize bot API: %v", err)
	}

	bot.Debug = true
	log.Printf("authorized as @%s", bot.Self.UserName)

	// Connect to MongoDB
	mongoClient, err := connectMongoDB(cfg.MongoURI)
	if err != nil {
		log.Panicf("failed to connect to MongoDB: %v", err)
	}

	return &BotService{
		api:        bot,
		gemini:     NewGeminiService(cfg.GeminiAPIKey),
		botMention: "@" + bot.Self.UserName,
		id:         bot.Self.ID,
		db:         mongoClient.Database("telegram_bot"),
	}
}

func connectMongoDB(uri string) (*mongo.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clientOptions := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		return nil, err
	}

	// Ping the database to verify connection
	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}

	log.Println("Connected to MongoDB successfully")
	return client, nil
}

func (bs *BotService) Run() {
	// Create indexes for messages collection for efficient queries
	bs.createMessageIndexes()

	updates := bs.api.GetUpdatesChan(tgbotapi.NewUpdate(0))
	for update := range updates {
		bs.handleUpdate(update)
	}
}

func (bs *BotService) createMessageIndexes() {
	// Create index on chat_id and timestamp for efficient queries
	messagesCollection := bs.db.Collection("messages")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := messagesCollection.Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys: bson.D{
				{Key: "chat_id", Value: 1},
				{Key: "timestamp", Value: -1},
			},
		},
	)

	if err != nil {
		log.Printf("Error creating index: %v", err)
	}
}

func (bs *BotService) handleUpdate(update tgbotapi.Update) {
	if update.Message == nil {
		return
	}

	// Store message in MongoDB (all messages in the chat)
	bs.storeMessage(update.Message)

	if update.Message.IsCommand() {
		bs.handleCommand(update.Message)
	} else if bs.isBotMentioned(update.Message.Text) {
		bs.handleQuery(update.Message)
	} else if update.Message.ReplyToMessage != nil && update.Message.ReplyToMessage.From.ID == bs.id {
		bs.handleQuery(update.Message)
	}
}

func (bs *BotService) storeMessage(msg *tgbotapi.Message) {
	if msg.Text == "" {
		return // Skip empty messages
	}

	username := ""
	firstName := ""
	lastName := ""

	if msg.From != nil {
		username = msg.From.UserName
		firstName = msg.From.FirstName
		lastName = msg.From.LastName
	}

	message := Message{
		ChatID:        msg.Chat.ID,
		MessageID:     msg.MessageID,
		FromUsername:  username,
		FromFirstName: firstName,
		FromLastName:  lastName,
		Text:          msg.Text,
		Timestamp:     msg.Time(),
	}

	messagesCollection := bs.db.Collection("messages")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := messagesCollection.InsertOne(ctx, message)
	if err != nil {
		log.Printf("Error storing message in MongoDB: %v", err)
	}
}

func (bs *BotService) handleCommand(msg *tgbotapi.Message) {
	response := tgbotapi.NewMessage(msg.Chat.ID, "")

	switch msg.Command() {
	case "start":
		response.Text = fmt.Sprintf("Hello! I'm ChatBuddy, your AI companion. Mention me with %s to chat, or use /help for more info!", bs.botMention)
	case "help":
		response.Text = fmt.Sprintf(botHelpMessage, bs.botMention, bs.botMention)
	case "summary":
		// Send initial message to let user know we're processing
		processingMsg := tgbotapi.NewMessage(msg.Chat.ID, fetchingMessagesMsg)
		processingMsg.ReplyToMessageID = msg.MessageID
		bs.sendResponse(processingMsg)

		// Process summary request asynchronously
		go bs.handleSummaryRequest(msg)
		return
	default:
		response.Text = unknownCmdMsg
	}
	bs.sendResponse(response)
}

func (bs *BotService) handleSummaryRequest(msg *tgbotapi.Message) {
	messages, err := bs.fetchMessagesFromDB(msg.Chat.ID, maxMessagesToFetch)
	if err != nil {
		errorMsg := tgbotapi.NewMessage(msg.Chat.ID, "Failed to fetch messages: "+err.Error())
		errorMsg.ReplyToMessageID = msg.MessageID
		bs.sendResponse(errorMsg)
		return
	}

	if len(messages) == 0 {
		noMsgReply := tgbotapi.NewMessage(msg.Chat.ID, "No recent messages found to summarize.")
		noMsgReply.ReplyToMessageID = msg.MessageID
		bs.sendResponse(noMsgReply)
		return
	}

	summary := bs.summarizeMessages(messages)

	response := tgbotapi.NewMessage(msg.Chat.ID, summary)
	response.ReplyToMessageID = msg.MessageID
	bs.sendResponse(response)
}

func (bs *BotService) fetchMessagesFromDB(chatID int64, limit int) ([]string, error) {
	messagesCollection := bs.db.Collection("messages")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Define query to get messages from the specific chat
	filter := bson.M{"chat_id": chatID}

	// Set options for sorting by timestamp descending and limit
	findOptions := options.Find()
	findOptions.SetSort(bson.D{{Key: "timestamp", Value: -1}})
	findOptions.SetLimit(int64(limit))

	// Execute query
	cursor, err := messagesCollection.Find(ctx, filter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("database query error: %w", err)
	}
	defer cursor.Close(ctx)

	// Decode messages
	var dbMessages []Message
	if err := cursor.All(ctx, &dbMessages); err != nil {
		return nil, fmt.Errorf("error decoding messages: %w", err)
	}

	// Convert to string format
	var messages []string
	for i := len(dbMessages) - 1; i >= 0; i-- { // Reverse to get chronological order
		msg := dbMessages[i]

		// Format username for display
		username := "Unknown"
		if msg.FromUsername != "" {
			username = "@" + msg.FromUsername
		} else if msg.FromFirstName != "" {
			username = msg.FromFirstName
			if msg.FromLastName != "" {
				username += " " + msg.FromLastName
			}
		}

		timestamp := msg.Timestamp.Format("2006-01-02 15:04:05")
		formattedMsg := fmt.Sprintf("[%s] %s: %s", timestamp, username, msg.Text)
		messages = append(messages, formattedMsg)
	}

	return messages, nil
}

func (bs *BotService) summarizeMessages(messages []string) string {
	combinedMessages := strings.Join(messages, "\n")

	prompt := fmt.Sprintf(`Below are the latest %d messages from a Telegram chat. Please provide a concise summary of the main topics and conversations:

%s

Summary instructions:
1. Identify the main topics discussed
2. Note any questions asked and answers given
3. Highlight any decisions made or important information shared
4. Keep the summary concise but informative
5. Format the summary in plain text (no markdown)
Response language: Same as the user's message`, len(messages), combinedMessages)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second) // Longer timeout for processing many messages
	defer cancel()

	resp, err := bs.gemini.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("gemini summarization error: %v", err)
		return "I couldn't generate a summary due to an error. Please try again later."
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "I couldn't generate a summary from these messages."
	}

	if text, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
		return string(text)
	}
	return "Error processing the summary response."
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) // 60s timeout
	defer cancel()

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
	return fmt.Sprintf(`You are a helpful and witty Telegram bot. The user asked: "%s"

    Follow these response guidelines:
    1. Keep all responses brief and concise (2-3 sentences maximum)
    2. DO NOT use markdown formatting (no asterisks for bold/italic)
    3. Be conversational and friendly
    4. Focus only on the most essential information
    5. Learn from the user's instructions and feedback during this conversation and adapt your responses accordingly.
    Response language: Same as the user's message`, sanitizeInput(query))
}

func sanitizeInput(input string) string {
	return strings.ReplaceAll(input, "%", "%%")
}

func (bs *BotService) sendResponse(response tgbotapi.MessageConfig) {
	text := response.Text
	maxLength := 4096

	for i := 0; i < len(text); i += maxLength {
		end := i + maxLength
		if end > len(text) {
			end = len(text)
		}

		chunk := tgbotapi.NewMessage(response.ChatID, text[i:end])
		chunk.ReplyToMessageID = response.ReplyToMessageID
		if _, err := bs.api.Send(chunk); err != nil {
			log.Printf("failed to send message chunk: %v", err)
		}
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
