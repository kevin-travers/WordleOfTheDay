package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// secretToken stands in for the token part of a Discord webhook URL.
// Tests check it never appears in an error message.
const secretToken = "SECRET-WEBHOOK-TOKEN"

func TestMain(m *testing.M) {
	// The code logs every word it picks; keep test output readable.
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeDiscord is a webhook endpoint that records what it receives and replies
// with a fixed status code and body.
type fakeDiscord struct {
	*httptest.Server

	status int
	body   string

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	method      string
	contentType string
	payload     WebhookPayload
}

func newFakeDiscord(t *testing.T, status int, body string) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{status: status, body: body}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("fake discord: request body is not valid JSON: %v", err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{method: r.Method, contentType: r.Header.Get("Content-Type"), payload: p})
		f.mu.Unlock()

		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.Close)
	return f
}

// webhookURL returns a URL on the fake server shaped like a real Discord webhook.
func (f *fakeDiscord) webhookURL() string {
	return f.URL + "/api/webhooks/123/" + secretToken
}

func (f *fakeDiscord) received() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// newFakeSSM starts a server that speaks just enough of the SSM API (JSON 1.1 protocol)
// for GetParameter, and returns a real *ssm.Client pointed at it.
// The respond function decides the reply for each GetParameter request.
func newFakeSSM(t *testing.T, respond func(in getParameterRequest) (status int, body string)) *ssm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Target"); got != "AmazonSSM.GetParameter" {
			t.Errorf("fake SSM: unexpected operation %q", got)
		}
		var in getParameterRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("fake SSM: bad request body: %v", err)
		}
		status, body := respond(in)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	return ssm.New(ssm.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("test", "test", ""),
		RetryMaxAttempts: 1, // fail fast instead of retrying error responses
	})
}

type getParameterRequest struct {
	Name           string
	WithDecryption bool
}

// parameterResponse builds a successful GetParameter reply.
func parameterResponse(name, value string) string {
	b, _ := json.Marshal(map[string]any{
		"Parameter": map[string]any{"Name": name, "Type": "SecureString", "Value": value},
	})
	return string(b)
}

// assertNoSecret fails the test if the error message contains the webhook token.
func assertNoSecret(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), secretToken) {
		t.Errorf("error message leaks the webhook token: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Word list
// ---------------------------------------------------------------------------

func TestWordsList(t *testing.T) {
	if len(words) == 0 {
		t.Fatal("words.txt is empty: wordForDate would panic")
	}

	fiveLowercase := regexp.MustCompile(`^[a-z]{5}$`)
	seen := make(map[string]bool, len(words))
	for i, w := range words {
		if !fiveLowercase.MatchString(w) {
			t.Errorf("word %d %q is not 5 lowercase letters", i+1, w)
		}
		if seen[w] {
			t.Errorf("word %q appears more than once", w)
		}
		seen[w] = true
	}
}

func TestWordForDateSameDaySameWord(t *testing.T) {
	// Every time on the same UTC day must give the same word: this is what stops a
	// retried or duplicated run from posting a different word.
	day := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	want := wordForDate(day)

	for _, offset := range []time.Duration{time.Second, 13 * time.Hour, 13*time.Hour + 5*time.Minute, 23*time.Hour + 59*time.Minute + 59*time.Second} {
		if got := wordForDate(day.Add(offset)); got != want {
			t.Errorf("at %s got %q, want %q (same UTC day)", day.Add(offset).Format(time.RFC3339), got, want)
		}
	}
}

func TestWordForDateUsesUTCDay(t *testing.T) {
	// 2026-11-24 20:00 in New York is 2026-11-25 01:00 UTC, so it must match the 25th.
	ny := time.FixedZone("EST", -5*60*60)
	local := time.Date(2026, 11, 24, 20, 0, 0, 0, ny)
	utc25 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	if got, want := wordForDate(local), wordForDate(utc25); got != want {
		t.Errorf("wordForDate(%s) = %q, want %q (the UTC day's word)", local.Format(time.RFC3339), got, want)
	}
}

func TestWordForDateIsStable(t *testing.T) {
	// Pins today's algorithm and word list. If this fails, the word picked for every
	// day has changed: expected after editing words.txt (update the want values),
	// a bug otherwise.
	tests := []struct {
		date time.Time
		want string
	}{
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "moruk"},
		{time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), "frits"},
	}
	for _, tt := range tests {
		if got := wordForDate(tt.date); got != tt.want {
			t.Errorf("wordForDate(%s) = %q, want %q", tt.date.Format(time.DateOnly), got, tt.want)
		}
	}
}

func TestWordForDateVariesAcrossDays(t *testing.T) {
	inList := make(map[string]bool, len(words))
	for _, w := range words {
		inList[w] = true
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	picked := make(map[string]bool)
	for d := range 365 {
		w := wordForDate(start.AddDate(0, 0, d))
		if !inList[w] {
			t.Fatalf("day %d gave %q, which is not in words.txt", d, w)
		}
		picked[w] = true
	}

	// A year of days out of ~15k words should give nearly all different words.
	// This catches a broken picker that returns the same word for every day.
	if len(picked) < 350 {
		t.Errorf("365 days gave only %d different words; picker looks broken", len(picked))
	}
}

// ---------------------------------------------------------------------------
// sendDiscordMessage
// ---------------------------------------------------------------------------

func TestSendDiscordMessageSuccess(t *testing.T) {
	discord := newFakeDiscord(t, http.StatusNoContent, "")
	payload := WebhookPayload{Content: "hello", Username: "WordleBot", AvatarURL: "https://example.com/a.png"}

	if err := sendDiscordMessage(t.Context(), discord.webhookURL(), payload); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := discord.received()
	if len(reqs) != 1 {
		t.Fatalf("discord received %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.contentType)
	}
	if got.payload != payload {
		t.Errorf("payload = %+v, want %+v", got.payload, payload)
	}
}

func TestSendDiscordMessageOmitsEmptyFields(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	if err := sendDiscordMessage(t.Context(), srv.URL, WebhookPayload{Content: "only content"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, field := range []string{"username", "avatar_url"} {
		if _, ok := raw[field]; ok {
			t.Errorf("empty %q was sent; omitempty should leave it out", field)
		}
	}
}

func TestSendDiscordMessageErrorStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantInErr []string
	}{
		{
			name:      "invalid token includes discord's reason",
			status:    http.StatusUnauthorized,
			body:      `{"message": "Invalid Webhook Token", "code": 50027}`,
			wantInErr: []string{"401", "Invalid Webhook Token"},
		},
		{
			name:      "unknown webhook",
			status:    http.StatusNotFound,
			body:      `{"message": "Unknown Webhook", "code": 10015}`,
			wantInErr: []string{"404", "Unknown Webhook"},
		},
		{
			name:      "rate limited",
			status:    http.StatusTooManyRequests,
			body:      `{"message": "You are being rate limited.", "retry_after": 1.5}`,
			wantInErr: []string{"429", "rate limited"},
		},
		{
			// Discord returns 200 instead of 204 only when ?wait=true is used, which
			// this code never does, so anything but 204 counts as a failure.
			name:      "200 is not treated as success",
			status:    http.StatusOK,
			body:      `{}`,
			wantInErr: []string{"200"},
		},
		{
			name:      "empty body still reports the status",
			status:    http.StatusInternalServerError,
			body:      "",
			wantInErr: []string{"500"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discord := newFakeDiscord(t, tt.status, tt.body)
			err := sendDiscordMessage(t.Context(), discord.webhookURL(), WebhookPayload{Content: "x"})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			for _, want := range tt.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			assertNoSecret(t, err)
		})
	}
}

func TestSendDiscordMessageTruncatesLongErrorBody(t *testing.T) {
	// "q" doesn't appear in the error's own wording, so counting it counts body bytes only.
	discord := newFakeDiscord(t, http.StatusBadRequest, strings.Repeat("q", 5000))

	err := sendDiscordMessage(t.Context(), discord.webhookURL(), WebhookPayload{Content: "x"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if got := strings.Count(err.Error(), "q"); got != 512 {
		t.Errorf("error includes %d bytes of the body, want exactly 512", got)
	}
}

func TestSendDiscordMessageRedactsURL(t *testing.T) {
	// A server that never answers, for the timeout case. Its handler is released at
	// cleanup (cleanups run last-registered-first, so close(release) runs before
	// slow.Close, which waits for in-flight handlers).
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })

	// A server that is closed before the request, for the connection-refused case.
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	tests := []struct {
		name     string
		url      string
		timeout  time.Duration
		wantIs   error  // errors.Is target, if any
		wantText string // text the redacted error must still contain
	}{
		{
			name:     "timeout",
			url:      slow.URL + "/api/webhooks/123/" + secretToken,
			timeout:  50 * time.Millisecond,
			wantIs:   context.DeadlineExceeded,
			wantText: "Post [webhook URL redacted]",
		},
		{
			name:     "connection refused",
			url:      closedURL + "/api/webhooks/123/" + secretToken,
			wantText: "Post [webhook URL redacted]",
		},
		{
			name:     "URL that fails to parse",
			url:      "https://bad host/api/webhooks/123/" + secretToken,
			wantText: "failed to build request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}

			err := sendDiscordMessage(ctx, tt.url, WebhookPayload{Content: "x"})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			assertNoSecret(t, err)
			if !strings.Contains(err.Error(), "[webhook URL redacted]") {
				t.Errorf("error %q is missing the redaction marker", err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not contain %q", err, tt.wantText)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("errors.Is(err, %v) = false; redaction must keep the underlying error", tt.wantIs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// redactURL
// ---------------------------------------------------------------------------

func TestRedactURLLeavesOtherErrorsAlone(t *testing.T) {
	plain := errors.New("something else went wrong")
	if got := redactURL(plain); got != plain {
		t.Errorf("redactURL changed a non-URL error: got %v, want %v", got, plain)
	}
}

// ---------------------------------------------------------------------------
// getWebhookURL
// ---------------------------------------------------------------------------

func TestGetWebhookURL(t *testing.T) {
	const paramName = "/wordle/discord-webhook-url"
	const realURL = "https://discord.com/api/webhooks/123/" + secretToken

	tests := []struct {
		name      string
		status    int
		body      string
		want      string
		wantInErr string
	}{
		{
			name:   "returns the stored URL",
			status: http.StatusOK,
			body:   parameterResponse(paramName, realURL),
			want:   realURL,
		},
		{
			name:      "placeholder from terraform means not set yet",
			status:    http.StatusOK,
			body:      parameterResponse(paramName, webhookPlaceholder),
			wantInErr: "no webhook URL set yet",
		},
		{
			name:      "empty value means not set yet",
			status:    http.StatusOK,
			body:      parameterResponse(paramName, ""),
			wantInErr: "no webhook URL set yet",
		},
		{
			name:      "missing parameter",
			status:    http.StatusBadRequest,
			body:      `{"__type": "ParameterNotFound", "message": "not found"}`,
			wantInErr: "ParameterNotFound",
		},
		{
			name:      "access denied",
			status:    http.StatusBadRequest,
			body:      `{"__type": "AccessDeniedException", "message": "not authorized"}`,
			wantInErr: "AccessDeniedException",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got getParameterRequest
			client := newFakeSSM(t, func(in getParameterRequest) (int, string) {
				got = in
				return tt.status, tt.body
			})

			url, err := getWebhookURL(t.Context(), client, paramName)

			if got.Name != paramName {
				t.Errorf("requested parameter %q, want %q", got.Name, paramName)
			}
			if !got.WithDecryption {
				t.Error("WithDecryption was false; a SecureString would come back encrypted")
			}

			if tt.wantInErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got URL %q", tt.wantInErr, url)
				}
				if !strings.Contains(err.Error(), tt.wantInErr) {
					t.Errorf("error %q does not contain %q", err, tt.wantInErr)
				}
				if !strings.Contains(err.Error(), paramName) {
					t.Errorf("error %q should name the parameter", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if url != tt.want {
				t.Errorf("got %q, want %q", url, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// postWordOfTheDay and the Lambda handler
// ---------------------------------------------------------------------------

func TestPostWordOfTheDay(t *testing.T) {
	discord := newFakeDiscord(t, http.StatusNoContent, "")
	now := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)

	if err := postWordOfTheDay(t.Context(), discord.webhookURL(), now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqs := discord.received()
	if len(reqs) != 1 {
		t.Fatalf("discord received %d requests, want 1", len(reqs))
	}
	p := reqs[0].payload
	if p.Username != botName {
		t.Errorf("username = %q, want %q", p.Username, botName)
	}
	if want := formatMessage(wordForDate(now)); p.Content != want {
		t.Errorf("content = %q, want %q", p.Content, want)
	}
}

func TestFormatMessageBoldUppercase(t *testing.T) {
	got := formatMessage("frits")
	if !strings.Contains(got, "**FRITS**") {
		t.Errorf("message %q should contain the word as bold uppercase **FRITS**", got)
	}
	if strings.Contains(got, "frits") {
		t.Errorf("message %q still contains the lowercase word", got)
	}
}

func TestPostWordOfTheDayRepeatsSameWordSameDay(t *testing.T) {
	// Simulates a retry: two runs on the same day must post identical messages.
	discord := newFakeDiscord(t, http.StatusNoContent, "")
	first := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	retry := first.Add(3 * time.Minute)

	for _, now := range []time.Time{first, retry} {
		if err := postWordOfTheDay(t.Context(), discord.webhookURL(), now); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	reqs := discord.received()
	if len(reqs) != 2 {
		t.Fatalf("discord received %d requests, want 2", len(reqs))
	}
	if reqs[0].payload.Content != reqs[1].payload.Content {
		t.Errorf("retry posted a different message:\n first: %q\n retry: %q", reqs[0].payload.Content, reqs[1].payload.Content)
	}
}

func TestPostWordOfTheDayReturnsDiscordError(t *testing.T) {
	discord := newFakeDiscord(t, http.StatusUnauthorized, `{"message": "Invalid Webhook Token"}`)

	err := postWordOfTheDay(t.Context(), discord.webhookURL(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected a 401 error, got %v", err)
	}
	assertNoSecret(t, err)
}

func TestHandlerEndToEnd(t *testing.T) {
	const paramName = "/wordle/discord-webhook-url"
	discord := newFakeDiscord(t, http.StatusNoContent, "")
	client := newFakeSSM(t, func(in getParameterRequest) (int, string) {
		return http.StatusOK, parameterResponse(in.Name, discord.webhookURL())
	})

	if err := newHandler(client, paramName)(t.Context()); err != nil {
		t.Fatalf("handler failed: %v", err)
	}
	if n := len(discord.received()); n != 1 {
		t.Errorf("discord received %d requests, want 1", n)
	}
}

func TestHandlerDoesNotPostWhenURLNotSet(t *testing.T) {
	discord := newFakeDiscord(t, http.StatusNoContent, "")
	client := newFakeSSM(t, func(in getParameterRequest) (int, string) {
		return http.StatusOK, parameterResponse(in.Name, webhookPlaceholder)
	})

	err := newHandler(client, "/wordle/discord-webhook-url")(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no webhook URL set yet") {
		t.Fatalf("expected a not-set-yet error, got %v", err)
	}
	if n := len(discord.received()); n != 0 {
		t.Errorf("discord received %d requests, want 0", n)
	}
}
