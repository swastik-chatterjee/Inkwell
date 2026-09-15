package tests

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "sync/atomic"
    "testing"
    "time"

    "markdown-social/internal/moderation"
)

// mistralBody builds a Mistral-shaped chat completion response whose single
// choice carries the given JSON verdict.
func mistralBody(t *testing.T, verdict string) string {
    t.Helper()
    return verdictBody(t, verdict, 0.93)
}

func verdictBody(t *testing.T, decision string, confidence float64) string {
    t.Helper()
    inner, err := json.Marshal(map[string]any{"decision": decision, "confidence": confidence, "reason": "stub verdict"})
    if err != nil {
        t.Fatalf("marshal verdict: %v", err)
    }
    outer, err := json.Marshal(map[string]any{
        "choices": []any{
            map[string]any{
                "index":         0,
                "message":       map[string]any{"role": "assistant", "content": string(inner)},
                "finish_reason": "stop",
            },
        },
    })
    if err != nil {
        t.Fatalf("marshal response: %v", err)
    }
    return string(outer)
}

func moderationWithStub(t *testing.T, handler http.HandlerFunc, clientTimeout time.Duration) *moderation.Service {
    t.Helper()
    server := httptest.NewServer(handler)
    t.Cleanup(server.Close)
    return moderation.New(&http.Client{Timeout: clientTimeout}, "test-key", "test-model", server.URL, nil)
}

func TestModerationAllowsCleanContent(t *testing.T) {
    var gotAuth, gotModel, gotContent, gotFormat string
    svc := moderationWithStub(t, func(w http.ResponseWriter, r *http.Request) {
        gotAuth = r.Header.Get("Authorization")
        var payload struct {
            Model          string `json:"model"`
            ResponseFormat *struct {
                Type string `json:"type"`
            } `json:"response_format"`
            Messages []struct {
                Role    string `json:"role"`
                Content string `json:"content"`
            } `json:"messages"`
        }
        if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
            t.Errorf("decode request: %v", err)
            w.WriteHeader(http.StatusBadRequest)
            return
        }
        gotModel = payload.Model
        if payload.ResponseFormat != nil {
            gotFormat = payload.ResponseFormat.Type
        }
        if len(payload.Messages) > 1 {
            gotContent = payload.Messages[len(payload.Messages)-1].Content
        }
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write([]byte(mistralBody(t, "allow")))
    }, 5*time.Second)

    result, err := svc.Moderate(context.Background(), "Nice post, thanks for sharing.")
    if err != nil {
        t.Fatalf("Moderate: unexpected error: %v", err)
    }
    if result.Decision != moderation.DecisionAllow {
        t.Fatalf("decision = %v, want allow", result.Decision)
    }
    if result.Confidence != 0.93 {
        t.Fatalf("confidence = %v, want 0.93", result.Confidence)
    }
    if gotAuth != "Bearer test-key" {
        t.Errorf("Authorization header = %q, want Bearer test-key", gotAuth)
    }
    if gotModel != "test-model" {
        t.Errorf("model = %q, want test-model", gotModel)
    }
    if gotFormat != "json_object" {
        t.Errorf("response_format = %q, want json_object", gotFormat)
    }
    // Privacy: only the classified content travels to the API.
    if gotContent != "Nice post, thanks for sharing." {
        t.Errorf("user message = %q, want exactly the submitted content", gotContent)
    }
}

func TestModerationRejectsFlaggedVerdict(t *testing.T) {
    svc := moderationWithStub(t, func(w http.ResponseWriter, r *http.Request) {
        _, _ = w.Write([]byte(mistralBody(t, "reject")))
    }, 5*time.Second)

    result, err := svc.Moderate(context.Background(), "Some rude text.")
    if err != nil {
        t.Fatalf("Moderate: unexpected error: %v", err)
    }
    if result.Decision != moderation.DecisionReject {
        t.Fatalf("decision = %v, want reject", result.Decision)
    }
    if result.Reason != "stub verdict" {
        t.Fatalf("reason = %q, want %q", result.Reason, "stub verdict")
    }
}

func TestModerationDecisionIsCaseInsensitive(t *testing.T) {
    svc := moderationWithStub(t, func(w http.ResponseWriter, r *http.Request) {
        _, _ = w.Write([]byte(mistralBody(t, "ALLOW")))
    }, 5*time.Second)
    result, err := svc.Moderate(context.Background(), "hello")
    if err != nil {
        t.Fatalf("Moderate: unexpected error: %v", err)
    }
    if result.Decision != moderation.DecisionAllow {
        t.Fatalf("decision = %v, want allow (case-insensitive)", result.Decision)
    }
}

func TestModerationDeterministicBlocklist(t *testing.T) {
    // A closed server: any HTTP call fails. The deterministic check must
    // reject unambiguous threats without contacting the API at all.
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
    server.Close()
    svc := moderation.New(&http.Client{Timeout: time.Second}, "key", "m", server.URL, nil)

    result, err := svc.Moderate(context.Background(), "Honestly, you should just kill yourself.")
    if err != nil {
        t.Fatalf("Moderate: unexpected error: %v", err)
    }
    if result.Decision != moderation.DecisionReject {
        t.Fatalf("decision = %v, want reject", result.Decision)
    }
    if result.Confidence != 1 {
        t.Fatalf("confidence = %v, want 1", result.Confidence)
    }

    // Content that is blunt but not hard-blocked must reach the API (and
    // therefore fail closed here, because the endpoint is dead).
    if _, err := svc.Moderate(context.Background(), "This idea is terrible and you should feel bad."); err == nil {
        t.Fatal("expected an error when the API is unreachable and no deterministic match fires")
    }
}

func TestModerationFailsClosedOnHTTPError(t *testing.T) {
    svc := moderationWithStub(t, func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusServiceUnavailable)
    }, 5*time.Second)
    result, err := svc.Moderate(context.Background(), "hello")
    if err == nil {
        t.Fatal("expected an error on HTTP failure")
    }
    if result.Decision != moderation.DecisionUnknown {
        t.Fatalf("decision = %v, want unknown", result.Decision)
    }
}

func TestModerationFailsClosedOnMalformedResponses(t *testing.T) {
    cases := []string{
        "not json at all",
        `{"choices":[]}`,
        `{"choices":[{"message":{"content":"also not json"}}]}`,
        `{"choices":[{"message":{"content":"{\"decision\":3}"}}]}`,
    }
    for _, body := range cases {
        svc := moderationWithStub(t, func(w http.ResponseWriter, r *http.Request) {
            _, _ = w.Write([]byte(body))
        }, time.Second)
        if _, err := svc.Moderate(context.Background(), "hello"); err == nil {
            t.Errorf("body %q: expected error, got none", body)
        }
    }
}

func TestModerationValidatesVerdictFields(t *testing.T) {
    cases := []struct {
        name      string
        decision  string
        confidence float64
    }{
        {"unknown decision", "maybe", 0.9},
        {"confidence above one", "allow", 1.5},
        {"confidence below zero", "allow", -0.1},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            body := verdictBody(t, tc.decision, tc.confidence)
            svc := moderationWithStub(t, func(w http.ResponseWriter, r *http.Request) {
                _, _ = w.Write([]byte(body))
            }, time.Second)
            if _, err := svc.Moderate(context.Background(), "hello"); err == nil {
                t.Errorf("decision %q confidence %v: expected error, got none", tc.decision, tc.confidence)
            }
        })
    }
}

func TestModerationTimeoutFailsClosed(t *testing.T) {
    svc := moderationWithStub(t, func(w http.ResponseWriter, r *http.Request) {
        time.Sleep(300 * time.Millisecond)
        _, _ = w.Write([]byte(mistralBody(t, "allow")))
    }, 50*time.Millisecond)
    if _, err := svc.Moderate(context.Background(), "hello"); err == nil {
        t.Fatal("expected an error when the API times out")
    }
}

func TestModerationSkipsAPIForInvalidContent(t *testing.T) {
    var calls atomic.Int64
    svc := moderationWithStub(t, func(w http.ResponseWriter, r *http.Request) {
        calls.Add(1)
        _, _ = w.Write([]byte(mistralBody(t, "allow")))
    }, time.Second)

    if _, err := svc.Moderate(context.Background(), ""); err == nil {
        t.Fatal("expected an error for empty content")
    }
    if _, err := svc.Moderate(context.Background(), "   "); err == nil {
        t.Fatal("expected an error for blank content")
    }
    if _, err := svc.Moderate(context.Background(), strings.Repeat("a", 100_001)); err == nil {
        t.Fatal("expected an error for oversized content")
    }
    if got := calls.Load(); got != 0 {
        t.Fatalf("API called %d times for invalid content, want 0", got)
    }
}

func TestModerationFailsClosedWithoutAPIKey(t *testing.T) {
    var calls atomic.Int64
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        calls.Add(1)
        _, _ = w.Write([]byte(mistralBody(t, "allow")))
    }))
    t.Cleanup(server.Close)

    svc := moderation.New(&http.Client{Timeout: time.Second}, "", "m", server.URL, nil)
    if _, err := svc.Moderate(context.Background(), "hello"); err == nil {
        t.Fatal("expected an error when the API key is missing")
    }
    if got := calls.Load(); got != 0 {
        t.Fatalf("API called %d times without a key, want 0", got)
    }
}

func TestModerationDecisionStrings(t *testing.T) {
    cases := map[moderation.Decision]string{
        moderation.DecisionAllow:   "allow",
        moderation.DecisionReject:  "reject",
        moderation.DecisionUnknown: "unknown",
    }
    for decision, want := range cases {
        if got := decision.String(); got != want {
            t.Errorf("Decision(%d).String() = %q, want %q", decision, got, want)
        }
    }
}
