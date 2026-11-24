package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

//go:embed words.txt
var wordsFile string

// Parsed once at startup. That's once per cold start, not once per request, on Lambda.
var words = strings.Fields(wordsFile)

// Reused across invocations so connections to Discord can be kept alive.
// The timeout stops a hung request from running until the Lambda timeout.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// wordForDate returns the word for the given day. The same UTC calendar day always gives
// the same word, so a retried or duplicated invocation posts the same word again rather
// than a different one.
//
// It hashes the date ("2026-11-24") with FNV-1a, a fixed published algorithm, so the
// choice won't change between Go versions. It does change if words.txt changes.
// Words can repeat on different days; with ~15k words that's rare.
func wordForDate(t time.Time) string {
	h := fnv.New64a()
	h.Write([]byte(t.UTC().Format(time.DateOnly))) // never returns an error
	return words[h.Sum64()%uint64(len(words))]
}

type WebhookPayload struct {
	Content   string `json:"content,omitempty"`
	Username  string `json:"username,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// function to send messages to discord
func sendDiscordMessage(ctx context.Context, webhookURL string, payload WebhookPayload) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("failed to build request: %w", redactURL(err))
	}
	req.Header.Set("Content-Type", "application/json")

	response, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request to discord: %w", redactURL(err))
	}
	defer func() {
		// Drain the rest of the body so the connection can be reused, then close it.
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}()

	// discord returns 204 No Content on a successful webhook execution
	if response.StatusCode != http.StatusNoContent {
		// Include the start of the body: Discord explains errors there,
		// e.g. {"message": "Invalid Webhook Token", "code": 50027}.
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("unexpected status code from discord: %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// redactURL removes the request URL from net/http and net/url errors.
// Their messages normally include the full URL, e.g.
//
//	Post "https://discord.com/api/webhooks/<id>/<token>": context deadline exceeded
//
// and the webhook token is a secret that must not reach CloudWatch logs.
func redactURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s [webhook URL redacted]: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

// function to get webhook url from parameter store.
// WithDecryption is required because the parameter is a SecureString;
// without it SSM returns the encrypted value instead of the URL.
func getWebhookURL(ctx context.Context, client *ssm.Client, paramName string) (string, error) {
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(paramName),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("failed to read SSM parameter %s: %w", paramName, err)
	}

	webhookURL := aws.ToString(out.Parameter.Value)
	if webhookURL == "" || webhookURL == webhookPlaceholder {
		return "", fmt.Errorf("SSM parameter %s has no webhook URL set yet", paramName)
	}
	return webhookURL, nil
}

// webhookPlaceholder is the value Terraform creates the SSM parameter with, before the
// real URL is set by hand. It MUST match value_wo on aws_ssm_parameter.webhook_url in
// infra/main.tf; if they differ, the "not set yet" check above stops working.
const webhookPlaceholder = "REPLACE_ME"

// Bot name and message posted to discord. messageTemplate's %s is replaced with the word,
// formatted by formatMessage.
const (
	botName         = "Wordy McWordface"
	messageTemplate = "The starting word of the day is: %s. Or don't. I'm a bot, I can't make you."
)

// formatMessage builds the discord message, with the word in bold uppercase.
// **...** is discord markdown for bold.
func formatMessage(word string) string {
	return fmt.Sprintf(messageTemplate, "**"+strings.ToUpper(word)+"**")
}

// postWordOfTheDay posts the word for the given day to discord.
// Callers pass time.Now(); tests pass a fixed time.
func postWordOfTheDay(ctx context.Context, webhookURL string, now time.Time) error {
	word := wordForDate(now)
	log.Printf("word of the day for %s: %s", now.UTC().Format(time.DateOnly), word)

	// TODO: add an avatar url
	payload := WebhookPayload{
		Content:  formatMessage(word),
		Username: botName,
	}

	if err := sendDiscordMessage(ctx, webhookURL, payload); err != nil {
		return err
	}
	log.Println("message sent successfully")
	return nil
}

// handle lambda requests.
// The EventBridge schedule event carries nothing we need, so the handler takes no input.
// Returning an error marks the invocation as failed in CloudWatch. EventBridge invokes
// Lambda asynchronously, so Lambda itself retries a failed run up to 2 more times.
func newHandler(client *ssm.Client, paramName string) func(context.Context) error {
	return func(ctx context.Context) error {
		webhookURL, err := getWebhookURL(ctx, client, paramName)
		if err != nil {
			return err
		}
		return postWordOfTheDay(ctx, webhookURL, time.Now())
	}
}

func main() {
	// Lambda sets AWS_LAMBDA_RUNTIME_API. Without it, run once locally.
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") == "" {
		runLocal()
		return
	}

	paramName := os.Getenv("WEBHOOK_PARAM_NAME")
	if paramName == "" {
		log.Fatal("WEBHOOK_PARAM_NAME is not set")
	}

	// Load AWS config and create the SSM client once per cold start, not per invocation.
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	lambda.Start(newHandler(ssm.NewFromConfig(cfg), paramName))
}

// runLocal posts once using DISCORD_WEBHOOK_URL, so you can test without deploying.
func runLocal() {
	webhookURL := os.Getenv("DISCORD_WEBHOOK_URL")
	if webhookURL == "" {
		log.Fatal("set DISCORD_WEBHOOK_URL to run locally")
	}

	if err := postWordOfTheDay(context.Background(), webhookURL, time.Now()); err != nil {
		log.Fatalf("failed to post word of the day: %v", err)
	}
}
