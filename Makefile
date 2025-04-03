# Go parameters
APP_NAME = mybot
SRC_FILES = main.go config.go
OUTPUT_DIR = bin

# Run the bot
.PHONY: run
run:
	go run $(SRC_FILES)

# Build the project
.PHONY: build
build:
	mkdir -p $(OUTPUT_DIR)
	go build -o $(OUTPUT_DIR)/$(APP_NAME) $(SRC_FILES)
