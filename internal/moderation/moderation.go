// Package moderation integrates the Mistral API to classify user-generated
// content for harassment before it is published.
//
// Design notes:
//
//   - The HTTP client is injected by the caller; the service is never hidden
//     global state.
//   - Deterministic, application-side checks run first so a tiny set of
//     unambiguously abusive phrases is rejected without any network call.
//   - The model's verdict is validated strictly: it must be a JSON object with
//     a known decision and a confidence in [0,1]. Anything else — timeout,
//     HTTP error, malformed JSON, unknown decision — is returned as an error
//     and callers must fail closed (do not publish).
//   - Only the user-generated text being classified is sent to Mistral. No
//     credentials, session data, or account metadata ever leaves the server.
package moderation

import (
    "bytes"
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "log/slog"
    "math"
    "net/http"
    "strings"
    "time"
)

// DefaultEndpoint is the Mistral chat completions API.
const DefaultEndpoint = "https://api.mistral.ai/v1/chat/completions"

// Decision is the moderation verdict for a piece of content.
type Decision uint8

const (
    DecisionUnknown Decision = iota
    DecisionAllow
    DecisionReject
)

func (d Decision) String() string {
    switch d {
    case DecisionAllow:
        return "allow"
    case DecisionReject:
        return "reject"
    default:
        return "unknown"
    }
}

// Result is the outcome of a moderation check.
type Result struct {
    Decision   Decision
    Confidence float64
    Reason     string
}

// Service classifies content through the Mistral API.
type Service struct {
    client   *http.Client
    apiKey   string
    model    string
    endpoint string
    log      *slog.Logger
}

// New creates a moderation service. A nil client gets a default 15-second
// timeout; a nil logger falls back to slog.Default().
func New(client *http.Client, apiKey, model, endpoint string, log *slog.Logger) *Service {
    if client == nil {
        client = &http.Client{Timeout: 15 * time.Second}
    }
    if log == nil {
        log = slog.Default()
    }
    if endpoint == "" {
        endpoint = DefaultEndpoint
    }
    return &Service{
        client:   client,
        apiKey:   apiKey,
        model:    model,
        endpoint: endpoint,
        log:      log,
    }
}

const moderationSystemPrompt = `You are the harassment-detection filter for a small Markdown-based social network. Classify the user-supplied text below. Reply with JSON only, in exactly this shape: {"decision":"allow"|"reject","confidence":<number between 0 and 1>,"reason":"<one short sentence>"}. Reject when the text contains harassment or abuse: threats of violence, targeted personal attacks, hate speech, sexual harassment, doxxing, or malicious intimidation. Allow ordinary content, including strong opinions, technical disagreement, profanity and criticism, as long as it is not abusive toward a person or group. Your entire reply must be the JSON object and nothing else.`

// hardBlockedPhrases is an intentionally tiny, deterministic blocklist of
// unambiguous harassment/threat phrases. The Mistral classifier is the main
// filter; this list guarantees obvious threats are rejected even if the API
// misbehaves or is asked to reason about something else.
var hardBlockedPhrases = []string{
    "kill yourself",
    "go kill yourself",
    "you should die",
    "go die",
    "i will kill you",
    "i'll kill you",
    "i'm going to kill you",
}

// Moderate classifies content. A non-nil error means the check could not be
// completed; callers MUST treat that as "do not publish" (fail closed).
func (s *Service) Moderate(ctx context.Context, content string) (Result, error) {
    if strings.TrimSpace(content) == "" {
        return Result{}, errors.New("moderation: refusing to classify empty content")
    }
    if len(content) > 100_000 {
        return Result{}, errors.New("moderation: content exceeds maximum length")
    }

    if phrase, blocked := deterministicMatch(content); blocked {
        return Result{Decision: DecisionReject, Confidence: 1, Reason: "matched blocked phrase: " + phrase}, nil
    }

    if s.apiKey == "" {
        return Result{}, errors.New("moderation: API key is not configured")
    }

    payload := chatRequest{
        Model:       s.model,
        Temperature: 0,
        MaxTokens:   200,
        ResponseFormat: &responseFormat{
            Type: "json_object",
        },
        Messages: []chatMessage{
            {Role: "system", Content: moderationSystemPrompt},
            {Role: "user", Content: content},
        },
    }

    body, err := json.Marshal(payload)
    if err != nil {
        return Result{}, fmt.Errorf("moderation: encode request: %w", err)
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
    if err != nil {
        return Result{}, fmt.Errorf("moderation: build request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Accept", "application/json")
    req.Header.Set("Authorization", "Bearer "+s.apiKey)

    resp, err := s.client.Do(req)
    if err != nil {
        return Result{}, fmt.Errorf("moderation: request failed: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
        return Result{}, fmt.Errorf("moderation: endpoint returned status %d", resp.StatusCode)
    }

    var parsed chatResponse
    dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
    if err := dec.Decode(&parsed); err != nil {
        return Result{}, fmt.Errorf("moderation: decode response: %w", err)
    }
    if len(parsed.Choices) == 0 {
        return Result{}, errors.New("moderation: response contained no choices")
    }

    verdict, err := parseVerdict(parsed.Choices[0].Message.Content)
    if err != nil {
        return Result{}, err
    }
    return verdict, nil
}

// parseVerdict validates the model's JSON verdict instead of trusting it
// blindly.
func parseVerdict(content string) (Result, error) {
    var verdict struct {
        Decision   string  `json:"decision"`
        Confidence float64 `json:"confidence"`
        Reason     string  `json:"reason"`
    }
    if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &verdict); err != nil {
        return Result{}, fmt.Errorf("moderation: verdict is not valid JSON: %w", err)
    }
    if math.IsNaN(verdict.Confidence) || verdict.Confidence < 0 || verdict.Confidence > 1 {
        return Result{}, errors.New("moderation: confidence out of range")
    }
    switch strings.ToLower(strings.TrimSpace(verdict.Decision)) {
    case "allow":
        return Result{Decision: DecisionAllow, Confidence: verdict.Confidence, Reason: verdict.Reason}, nil
    case "reject":
        return Result{Decision: DecisionReject, Confidence: verdict.Confidence, Reason: verdict.Reason}, nil
    default:
        return Result{}, fmt.Errorf("moderation: unknown decision %q", verdict.Decision)
    }
}

func deterministicMatch(content string) (string, bool) {
    lower := strings.ToLower(content)
    for _, phrase := range hardBlockedPhrases {
        if strings.Contains(lower, phrase) {
            return phrase, true
        }
    }
    return "", false
}

type chatMessage struct {
    Role    string `json:"role"`
    Content string `json:"content"`
}

type responseFormat struct {
    Type string `json:"type"`
}

type chatRequest struct {
    Model          string          `json:"model"`
    Messages       []chatMessage   `json:"messages"`
    Temperature    float64         `json:"temperature"`
    MaxTokens      int             `json:"max_tokens,omitempty"`
    ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatResponse struct {
    Choices []struct {
        Index   int    `json:"index"`
        Message struct {
            Role    string `json:"role"`
            Content string `json:"content"`
        } `json:"message"`
        FinishReason string `json:"finish_reason"`
    } `json:"choices"`
}
