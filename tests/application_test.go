package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"

	"markdown-social/internal/auth"
	"markdown-social/internal/config"
	"markdown-social/internal/handlers"
	"markdown-social/internal/markdown"
	"markdown-social/internal/middleware"
	"markdown-social/internal/moderation"
	"markdown-social/internal/repository"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// mistralStub is a local Mistral API double. It counts calls and can be
// switched between allow / reject / HTTP-failure modes.
type mistralStub struct {
	calls      atomic.Int64
	fail       atomic.Bool
	reject     atomic.Bool
	allowBody  string
	rejectBody string
	endpoint   string
}

func verdictJSON(t *testing.T, decision string) string {
	t.Helper()
	inner, err := json.Marshal(map[string]any{"decision": decision, "confidence": 0.99, "reason": "stub"})
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

func newMistralStub(t *testing.T) *mistralStub {
	t.Helper()
	stub := &mistralStub{
		allowBody:  verdictJSON(t, "allow"),
		rejectBody: verdictJSON(t, "reject"),
	}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	stub.endpoint = server.URL
	return stub
}

func (s *mistralStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	w.Header().Set("Content-Type", "application/json")
	if s.fail.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body := s.allowBody
	if s.reject.Load() {
		body = s.rejectBody
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// newGitHubFake serves a token endpoint compatible with golang.org/x/oauth2.
func newGitHubFake(t *testing.T) *oauth2.Config {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.PostForm.Get("code") == "good-code" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"stub-token","token_type":"bearer"}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  server.URL + "/login/oauth/authorize",
			TokenURL: server.URL + "/login/oauth/access_token",
		},
		RedirectURL: "http://app.test/auth/github/callback",
		Scopes:      []string{"read:user"},
	}
}

// ---------------------------------------------------------------------------
// Application harness
// ---------------------------------------------------------------------------

type testApp struct {
	handler   http.Handler
	repo      *repository.Repository
	signer    *auth.Signer
	moderator *mistralStub
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	repo, _ := newTestDB(t)

	stub := newMistralStub(t)
	githubConf := newGitHubFake(t)
	signer := auth.NewSigner("unit-test-session-secret")

	cfg := &config.Config{
		AppURL:       "http://app.test",
		Port:         "8080",
		Environment:  config.EnvDevelopment,
		MistralModel: "test-model",
	}

	authSvc := auth.New(githubConf, repo, signer, false, slog.Default())
	renderer, err := markdown.New()
	if err != nil {
		t.Fatalf("markdown renderer: %v", err)
	}
	moderator := moderation.New(nil, "test-key", "test-model", stub.endpoint, slog.Default())

	h, err := handlers.New(handlers.Deps{
		Repo:        repo,
		Auth:        authSvc,
		Moderation:  moderator,
		Markdown:    renderer,
		Signer:      signer,
		Config:      cfg,
		Log:         slog.Default(),
		TemplateDir: "../web/templates",
		StaticDir:   "../web/static",
	})
	if err != nil {
		t.Fatalf("handlers: %v", err)
	}

	mw := middleware.New(slog.Default(), authSvc, false)
	return &testApp{handler: mw.Wrap(h.Routes()), repo: repo, signer: signer, moderator: stub}
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{Jar: jar}
}

// noRedirect returns a shallow copy that shares the jar but does not follow
// redirects, so 3xx responses can be inspected directly.
func noRedirect(client *http.Client) *http.Client {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

func doGet(t *testing.T, client *http.Client, target string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

func doPost(t *testing.T, client *http.Client, target string, values url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := client.PostForm(target, values)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`)

func extractCSRF(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CSRF token found in page (len=%d)", len(body))
	}
	return m[1]
}

func mustContain(t *testing.T, body, needle string) {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Errorf("page does not contain %q", needle)
	}
}

func mustNotContain(t *testing.T, body, needle string) {
	t.Helper()
	if strings.Contains(body, needle) {
		t.Errorf("page must not contain %q", needle)
	}
}

func jarSetSession(t *testing.T, client *http.Client, base, value string) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	client.Jar.SetCookies(u, []*http.Cookie{{Name: "ms_session", Value: value}})
}

// ---------------------------------------------------------------------------
// End-to-end application flow
// ---------------------------------------------------------------------------

func TestApplicationFlow(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	ts := httptest.NewServer(app.handler)
	t.Cleanup(ts.Close)
	base := ts.URL

	browser := newBrowser(t)  // becomes "octocat"
	nr := noRedirect(browser) // same jar, no redirect-following

	t.Run("anonymous visitor gets public feed and headers", func(t *testing.T) {
		resp, body := doGet(t, browser, base+"/")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		mustContain(t, body, "Recent posts")
		mustContain(t, body, "Sign in")
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("CSP = %q", csp)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("X-Frame-Options = %q, want DENY", got)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}

		resp, body = doGet(t, browser, base+"/login")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login status = %d", resp.StatusCode)
		}
		mustContain(t, body, "Continue with GitHub")

		resp, _ = doGet(t, browser, base+"/static/style.css")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("style.css status = %d", resp.StatusCode)
		}
		resp, _ = doGet(t, browser, base+"/static/app.js")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("app.js status = %d", resp.StatusCode)
		}
	})

	t.Run("oauth state is validated", func(t *testing.T) {
		resp, _ := doGet(t, nr, base+"/auth/github")
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", resp.StatusCode)
		}
		location := resp.Header.Get("Location")
		if !strings.Contains(location, "/login/oauth/authorize") {
			t.Fatalf("authorize redirect = %q", location)
		}
		u, err := url.Parse(location)
		if err != nil {
			t.Fatalf("parse location: %v", err)
		}
		state := u.Query().Get("state")
		if state == "" {
			t.Fatal("no state parameter in authorize redirect")
		}

		// Wrong state: rejected, no session issued.
		resp, _ = doGet(t, nr, base+"/auth/github/callback?code=good-code&state=wrong-state")
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Fatalf("wrong state: status=%d location=%q, want 303 /login", resp.StatusCode, resp.Header.Get("Location"))
		}
		_, body := doGet(t, browser, base+"/")
		mustContain(t, body, "try again")
		mustNotContain(t, body, "Log out")
	})

	t.Run("oauth token exchange failure is graceful", func(t *testing.T) {
		resp, _ := doGet(t, nr, base+"/auth/github")
		location := resp.Header.Get("Location")
		u, _ := url.Parse(location)
		state := u.Query().Get("state")

		resp, _ = doGet(t, nr, base+"/auth/github/callback?code=bad-code&state="+url.QueryEscape(state))
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Fatalf("bad code: status=%d location=%q, want 303 /login", resp.StatusCode, resp.Header.Get("Location"))
		}
	})

	t.Run("unusable github profile never issues a session", func(t *testing.T) {
		// The token endpoint succeeds, but the stub access token is not
		// accepted by the real GitHub user endpoint (401 online, network
		// error offline) — either way sign-in must fail safely.
		resp, _ := doGet(t, nr, base+"/auth/github")
		location := resp.Header.Get("Location")
		u, _ := url.Parse(location)
		state := u.Query().Get("state")

		resp, _ = doGet(t, nr, base+"/auth/github/callback?code=good-code&state="+url.QueryEscape(state))
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Fatalf("status=%d location=%q, want 303 /login", resp.StatusCode, resp.Header.Get("Location"))
		}
		_, body := doGet(t, browser, base+"/")
		mustNotContain(t, body, "Log out")
	})

	var octoToken string
	t.Run("authenticated session renders personalized nav", func(t *testing.T) {
		user, err := app.repo.UpsertGitHubUser(ctx, "octocat", 1001, "Octo Cat", "https://avatars.example/octo.png")
		if err != nil {
			t.Fatalf("upsert user: %v", err)
		}
		octoToken, _, err = app.repo.CreateSession(ctx, user.ID)
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		jarSetSession(t, browser, base, app.signer.Seal(octoToken))

		_, body := doGet(t, browser, base+"/")
		mustContain(t, body, "Octo Cat")
		mustContain(t, body, "Log out")
	})

	var csrf string
	t.Run("CSRF is enforced on POST", func(t *testing.T) {
		resp, _ := doPost(t, nr, base+"/posts", url.Values{"title": {"no csrf"}, "content": {"x"}})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("post creation is moderated then published", func(t *testing.T) {
		_, body := doGet(t, browser, base+"/posts/new")
		mustContain(t, body, "Write a post")
		csrf = extractCSRF(t, body)

		calls := app.moderator.calls.Load()
		resp, _ := doPost(t, nr, base+"/posts", url.Values{
			"csrf_token": {csrf},
			"title":      {"A first post"},
			"content":    {"Hello **world** from a test."},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "/posts/1" {
			t.Fatalf("location = %q, want /posts/1", got)
		}
		if got := app.moderator.calls.Load(); got != calls+1 {
			t.Fatalf("moderation calls = %d, want %d", got, calls+1)
		}

		_, body = doGet(t, browser, base+"/posts/1")
		mustContain(t, body, "A first post")
		mustContain(t, body, "<strong>world</strong>")
		mustContain(t, body, "Octo Cat")
	})

	t.Run("markdown is sanitized before storage", func(t *testing.T) {
		evil := "Safe text [bad](javascript:alert(1)) <script>alert(1)</script> <img src=x onerror=alert(1)>"
		resp, _ := doPost(t, nr, base+"/posts", url.Values{
			"csrf_token": {csrf},
			"title":      {"XSS attempt"},
			"content":    {evil},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", resp.StatusCode)
		}
		_, body := doGet(t, browser, base+"/posts/2")
		mustContain(t, body, "XSS attempt")
		mustNotContain(t, body, "<script")
		mustNotContain(t, body, "javascript:")
		mustNotContain(t, body, "<img")
	})

	t.Run("like and unlike", func(t *testing.T) {
		resp, _ := doPost(t, nr, base+"/posts/1/like", url.Values{"csrf_token": {csrf}})
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/posts/1" {
			t.Fatalf("like: status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
		}
		_, body := doGet(t, browser, base+"/posts/1")
		mustContain(t, body, "1 like")

		resp, _ = doPost(t, nr, base+"/posts/1/unlike", url.Values{"csrf_token": {csrf}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("unlike: status = %d", resp.StatusCode)
		}
		_, body = doGet(t, browser, base+"/posts/1")
		mustContain(t, body, "0 likes")
	})

	t.Run("comments are moderated markdown", func(t *testing.T) {
		calls := app.moderator.calls.Load()
		resp, _ := doPost(t, nr, base+"/posts/1/comments", url.Values{
			"csrf_token": {csrf},
			"content":    {"A *fine* comment."},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/posts/1#comment-") {
			t.Fatalf("location = %q", loc)
		}
		if got := app.moderator.calls.Load(); got != calls+1 {
			t.Fatalf("moderation calls = %d, want %d", got, calls+1)
		}
		_, body := doGet(t, browser, base+"/posts/1")
		mustContain(t, body, "<em>fine</em>")
	})

	t.Run("moderation rejection blocks publication", func(t *testing.T) {
		app.moderator.reject.Store(true)
		defer app.moderator.reject.Store(false)

		resp, body := doPost(t, browser, base+"/posts", url.Values{
			"csrf_token": {csrf},
			"title":      {"Should not be stored"},
			"content":    {"Some content."},
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		mustContain(t, body, "flagged by the automated content check")
		mustContain(t, body, "Should not be stored") // draft preserved

		_, total, err := app.repo.ListPosts(ctx, 0, 100, 0)
		if err != nil {
			t.Fatalf("list posts: %v", err)
		}
		if total != 2 {
			t.Fatalf("total posts = %d, want 2 (rejected post must not be stored)", total)
		}
	})

	t.Run("moderation outage fails closed", func(t *testing.T) {
		app.moderator.fail.Store(true)
		defer app.moderator.fail.Store(false)

		resp, body := doPost(t, browser, base+"/posts", url.Values{
			"csrf_token": {csrf},
			"title":      {"Also not stored"},
			"content":    {"Some content."},
		})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
		mustContain(t, body, "could not complete the safety check")

		_, total, err := app.repo.ListPosts(ctx, 0, 100, 0)
		if err != nil {
			t.Fatalf("list posts: %v", err)
		}
		if total != 2 {
			t.Fatalf("total posts = %d, want 2 (fail-closed must not store)", total)
		}
	})

	var csrf2, ghostToken string
	t.Run("authorization between users", func(t *testing.T) {
		ghost, err := app.repo.UpsertGitHubUser(ctx, "ghost", 1002, "Ghost User", "")
		if err != nil {
			t.Fatalf("upsert ghost: %v", err)
		}
		ghostToken, _, err = app.repo.CreateSession(ctx, ghost.ID)
		if err != nil {
			t.Fatalf("ghost session: %v", err)
		}
		browser2 := newBrowser(t)
		jarSetSession(t, browser2, base, app.signer.Seal(ghostToken))
		nr2 := noRedirect(browser2)

		_, body := doGet(t, browser2, base+"/posts/1")
		mustContain(t, body, "Ghost User")
		csrf2 = extractCSRF(t, body)

		// Ghost cannot edit or delete Octocat's post.
		resp, _ := doPost(t, nr2, base+"/posts/1/delete", url.Values{"csrf_token": {csrf2}})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("delete as ghost: status = %d, want 403", resp.StatusCode)
		}
		resp, _ = doPost(t, nr2, base+"/posts/1/edit", url.Values{
			"csrf_token": {csrf2}, "title": {"Hacked"}, "content": {"Hacked"},
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("edit as ghost: status = %d, want 403", resp.StatusCode)
		}
		if _, err := app.repo.GetPost(ctx, 1, 0); err != nil {
			t.Fatalf("post vanished: %v", err)
		}

		// Ghost may comment on Octocat's post.
		resp, _ = doPost(t, nr2, base+"/posts/1/comments", url.Values{
			"csrf_token": {csrf2}, "content": {"Ghost says hi"},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("ghost comment: status = %d, want 303", resp.StatusCode)
		}

		// Octocat cannot delete Ghost's comment (comment id 2).
		resp, _ = doPost(t, nr, base+"/comments/2/delete", url.Values{"csrf_token": {csrf}})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("octocat deletes ghost comment: status = %d, want 403", resp.StatusCode)
		}

		// Ghost deletes his own comment.
		resp, _ = doPost(t, nr2, base+"/comments/2/delete", url.Values{"csrf_token": {csrf2}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("ghost deletes own comment: status = %d, want 303", resp.StatusCode)
		}
		_, body = doGet(t, browser, base+"/posts/1")
		mustNotContain(t, body, "Ghost says hi")
	})

	t.Run("post editing and unchanged-edit moderation skip", func(t *testing.T) {
		resp, _ := doPost(t, nr, base+"/posts/1/edit", url.Values{
			"csrf_token": {csrf},
			"title":      {"A first post (edited)"},
			"content":    {"Hello **world** from a test."},
		})
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/posts/1" {
			t.Fatalf("edit: status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
		}
		_, body := doGet(t, browser, base+"/posts/1")
		mustContain(t, body, "A first post (edited)")

		// An edit that changes nothing must not call Mistral again.
		calls := app.moderator.calls.Load()
		resp, _ = doPost(t, nr, base+"/posts/1/edit", url.Values{
			"csrf_token": {csrf},
			"title":      {"A first post (edited)"},
			"content":    {"Hello **world** from a test."},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("unchanged edit: status = %d, want 303", resp.StatusCode)
		}
		if got := app.moderator.calls.Load(); got != calls {
			t.Fatalf("moderation calls = %d, want %d (unchanged content must skip the API)", got, calls)
		}
	})

	t.Run("profiles and 404s", func(t *testing.T) {
		resp, body := doGet(t, browser, base+"/users/octocat")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("profile status = %d", resp.StatusCode)
		}
		mustContain(t, body, "Octo Cat")
		mustContain(t, body, "@octocat")
		mustContain(t, body, "Joined")

		resp, _ = doGet(t, browser, base+"/users/nobody-here")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("missing profile status = %d, want 404", resp.StatusCode)
		}
		resp, _ = doGet(t, browser, base+"/posts/999999")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("missing post status = %d, want 404", resp.StatusCode)
		}
		resp, _ = doGet(t, browser, base+"/definitely-not-a-page")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown route status = %d, want 404", resp.StatusCode)
		}
		resp, _ = doGet(t, browser, base+"/logout")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET /logout status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("bio update renders sanitized markdown", func(t *testing.T) {
		_, body := doGet(t, browser, base+"/settings/profile")
		csrf = extractCSRF(t, body)

		resp, _ := doPost(t, nr, base+"/settings/profile", url.Values{
			"csrf_token": {csrf},
			"bio":        {"I **write** tests."},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("bio save: status = %d, want 303", resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "/users/octocat" {
			t.Fatalf("bio redirect = %q", got)
		}
		_, body = doGet(t, browser, base+"/users/octocat")
		mustContain(t, body, "<strong>write</strong>")
	})

	t.Run("account deletion requires explicit confirmation", func(t *testing.T) {
		_, body := doGet(t, browser, base+"/settings")
		csrf = extractCSRF(t, body)
		mustContain(t, body, "DELETE")

		resp, body := doPost(t, browser, base+"/settings/account/delete", url.Values{
			"csrf_token": {csrf},
			"confirm":    {"yes"},
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("wrong confirm: status = %d, want 400", resp.StatusCode)
		}
		mustContain(t, body, "Type DELETE")

		user, err := app.repo.GetUserByUsername(ctx, "ghost")
		if err != nil {
			t.Fatalf("ghost vanished after failed deletion: %v", err)
		}
		_ = user
	})

	t.Run("account deletion is destructive and complete", func(t *testing.T) {
		ghost, err := app.repo.GetUserByUsername(ctx, "ghost")
		if err != nil {
			t.Fatalf("lookup ghost: %v", err)
		}

		resp, _ := doPost(t, nr, base+"/settings/account/delete", url.Values{
			"csrf_token": {csrf},
			"confirm":    {"DELETE"},
		})
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("delete: status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
		}

		// Everything owned by octocat is gone.
		if _, err := app.repo.GetUserByUsername(ctx, "octocat"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("octocat still exists: %v", err)
		}
		if _, err := app.repo.GetPost(ctx, 1, 0); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("octocat's post still exists: %v", err)
		}
		if _, err := app.repo.UserForSession(ctx, octoToken); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("octocat's session still resolves: %v", err)
		}
		// Ghost's data survives.
		if _, err := app.repo.GetUserByID(ctx, ghost.ID); err != nil {
			t.Fatalf("ghost was deleted with octocat's account: %v", err)
		}

		// The browser is logged out.
		_, body := doGet(t, browser, base+"/")
		mustNotContain(t, body, "Log out")
		mustContain(t, body, "Sign in")
	})

	t.Run("logout invalidates the session", func(t *testing.T) {
		browser2 := newBrowser(t)
		jarSetSession(t, browser2, base, app.signer.Seal(ghostToken))
		_, body := doGet(t, browser2, base+"/settings")
		csrf2 = extractCSRF(t, body)

		resp, _ := doPost(t, noRedirect(browser2), base+"/logout", url.Values{"csrf_token": {csrf2}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("logout: status = %d, want 303", resp.StatusCode)
		}
		_, body = doGet(t, browser2, base+"/")
		mustNotContain(t, body, "Log out")
	})

	t.Run("feed pagination", func(t *testing.T) {
		ghost, err := app.repo.GetUserByUsername(ctx, "ghost")
		if err != nil {
			t.Fatalf("lookup ghost: %v", err)
		}
		for i := 1; i <= 12; i++ {
			_, err := app.repo.CreatePost(ctx, ghost.ID, fmt.Sprintf("Bulk post %d", i), "content", "<p>content</p>")
			if err != nil {
				t.Fatalf("bulk post %d: %v", i, err)
			}
		}
		_, body := doGet(t, browser, base+"/")
		mustContain(t, body, "Bulk post 12")
		mustContain(t, body, "page=2")

		resp, body := doGet(t, browser, base+"/?page=2")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page 2 status = %d", resp.StatusCode)
		}
		mustContain(t, body, "Page 2 of")
		mustContain(t, body, "Bulk post 2")
	})
}

// ---------------------------------------------------------------------------
// Markdown sanitization unit tests
// ---------------------------------------------------------------------------

func TestMarkdownRenderingPipeline(t *testing.T) {
	renderer, err := markdown.New()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}

	t.Run("dangerous markup is neutralized", func(t *testing.T) {
		input := "Hello <script>alert('xss')</script> [bad](javascript:alert(1)) <img src=x onerror=alert(1)> **ok**"
		out, err := renderer.Render(input)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		html := string(out)
		if !strings.Contains(html, "<strong>ok</strong>") {
			t.Errorf("safe formatting was lost: %q", html)
		}
		for _, forbidden := range []string{"<script", "javascript:", "<img", "onerror="} {
			if strings.Contains(html, forbidden) {
				t.Errorf("unsafe content survived: %q (found %q)", html, forbidden)
			}
		}
	})

	t.Run("safe images and links are kept, links get nofollow", func(t *testing.T) {
		out, err := renderer.Render("![alt](https://example.com/i.png) and [a link](https://example.com)")
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		html := string(out)
		if !strings.Contains(html, `<img src="https://example.com/i.png"`) {
			t.Errorf("safe image stripped: %q", html)
		}
		if !strings.Contains(html, `rel="nofollow"`) {
			t.Errorf("links must carry rel=nofollow: %q", html)
		}
	})

	t.Run("tables and code fences render", func(t *testing.T) {
		out, err := renderer.Render("| a | b |\n| --- | --- |\n| 1 | 2 |\n\n```go\nfmt.Println(1)\n```")
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		html := string(out)
		if !strings.Contains(html, "<table") {
			t.Errorf("GFM table missing: %q", html)
		}
		if !strings.Contains(html, "<pre") {
			t.Errorf("code block missing: %q", html)
		}
	})
}
