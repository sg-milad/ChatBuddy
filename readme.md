# Telegram AI ChatBot

This is a Telegram bot built with Golang that integrates with the Gemini AI API to generate responses to user messages. The bot only replies when mentioned in a group chat and supports intelligent responses based on message context.

## Features

- **AI-Powered Responses**: Uses the Gemini AI API for smart and contextual replies.
- **Mention-Based Replies**: Responds only when mentioned in group chats.
- **Dynamic Context Handling**: Answers based on previous messages when replying.
- **Easy Deployment**: Simple to run and deploy using `Makefile`.

## Installation & Setup

### Prerequisites

- Golang installed (`go version` to check)
- Telegram Bot API Token ([Create a bot](https://core.telegram.org/bots#botfather))
- Gemini API Key [get it here](https://aistudio.google.com/apikey)

### Clone the Repository

```sh
git clone https://github.com/yourusername/telegram-ai-bot.git
cd telegram-ai-bot
```

### Configuration

1. Create a `.env` file with:
   ```sh
   BOT_TOKEN=your_telegram_bot_token
   GEMINI_API_KEY=your_gemini_api_key
   ```

### Running the Bot

#### Using `go run`

```sh
go run main.go config.go
```

#### Using `Makefile`

```sh
make run
```

### Building the Bot

To compile the bot into a binary:

```sh
make build
```

The binary will be located in `bin/mybot`.

## Usage

```
  User: Mention the bot in a group: `@YourBot what is 2+2?`
  Bot: 4

  User: What is Golang?
  User:@YourBot(replay on the message)
  Bot: Golang is a programming language developed by Google...
```

## Contributing

Feel free to fork and submit a pull request!

## License

MIT License

---

Made with ❤️ in Golang 🚀
