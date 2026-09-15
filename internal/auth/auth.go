// Package auth implements GitHub OAuth 2.0 sign-in and cookie-based sessions.
//
// OAuth cryptography (state, code exchange, token handling) is delegated to
// golang.org/x/oauth2; nothing here implements OAuth by hand. Session tokens
// are 256-bit random values stored only as SHA-256 hashes; the cookie value is
// additionally signed with an HMAC derived from SESSION_SECRET so forged
// cookies never even reach the database.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"markdown-social/internal/models"
	"markdown-social/internal/repository"
)

const (
	sessionCookieName = "ms_session"
	stateCookieName   = "ms_oauth_state"
	stateTTL          = 10 * time.Minute
	exchangeTimeout   = 20 * time.Second
	githubUserTimeout = 10 * time.Second
	githubUserURL     = "https://api.github.com/user"
)

// ErrStateMismatch signals a failed OAuth state validation.
var ErrStateMismatch = errors.New("oauth state mismatch")

// Signer produces and verifies HMAC-SHA256 signatures for cookie values.
type Signer struct {
	secret []byte
}

// NewSigner creates a signer from SESSION_SECRET.
func NewSigner(secret string) *Signer {
	return &Signer{secret: []byte(secret)}
}

// Seal returns "value.signature".
func (s *Signer) Seal(value string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(value))
	return value + "." + hex.EncodeToString(mac.Sum(nil))
}

// Unseal verifies the signature and returns the original value.
func (s *Signer) Unseal(sealed string) (string, bool) {
	dot := strings.LastIndex(sealed, ".")
	if dot <= 0 {
		return "", false
	}
	value, signature := sealed[:dot], sealed[dot+1:]
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(value))
	expected := hex.EncodeToString(mac.Sum(nil))
	if len(signature) != len(expected) || subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return "", false
	}
	return value, true
}

// Service handles the GitHub login lifecycle and sessions.
type Service struct {
	oauth  *oauth2.Config
	repo   *repository.Repository
	signer *Signer
	secure bool
	log    *slog.Logger
}

// New creates an auth service. secure marks cookies Secure (production/HTTPS).
func New(oauth *oauth2.Config, repo *repository.Repository, signer *Signer, secure bool, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{oauth: oauth, repo: repo, signer: signer, secure: secure, log: log}
}

// BeginLogin redirects the browser to GitHub with a cryptographically random
// state stored in a short-lived cookie.
func (s *Service) BeginLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randomHex(16)
	if err != nil {
		http.Error(w, "could not start sign-in", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   int(stateTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.oauth.AuthCodeURL(state), http.StatusFound)
}

// CompleteLogin validates the OAuth callback, exchanges the authorization code
// for tokens, fetches the GitHub profile, upserts the local user, and creates
// a session cookie. Detailed errors are logged server-side and never shown to
// users.
func (s *Service) CompleteLogin(ctx context.Context, w http.ResponseWriter, r *http.Request) (*models.User, error) {
	// The state cookie is cleared no matter what happens next.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})

	q := r.URL.Query()
	if providerErr := q.Get("error"); providerErr != "" {
		return nil, fmt.Errorf("authorization denied by provider")
	}

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" {
		return nil, errors.New("missing oauth state cookie")
	}
	stateParam := q.Get("state")
	if len(stateParam) == 0 ||
		subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(stateParam)) != 1 {
		return nil, ErrStateMismatch
	}

	code := q.Get("code")
	if code == "" {
		return nil, errors.New("missing authorization code")
	}

	exchangeCtx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()

	token, err := s.oauth.Exchange(exchangeCtx, code)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}

	githubUser, err := fetchGitHubUser(exchangeCtx, s.oauth, token)
	if err != nil {
		return nil, err
	}

	user, err := s.repo.UpsertGitHubUser(ctx, githubUser.Login, githubUser.ID, githubUser.Name, githubUser.AvatarURL)
	if err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}

	sessionToken, expires, err := s.repo.CreateSession(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.signer.Seal(sessionToken),
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return user, nil
}

// CurrentUser resolves the session cookie to a user, or nil when the request
// is anonymous (or the cookie is invalid/expired).
func (s *Service) CurrentUser(ctx context.Context, r *http.Request) (*models.User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, nil
	}
	token, ok := s.signer.Unseal(cookie.Value)
	if !ok {
		return nil, nil
	}
	user, err := s.repo.UserForSession(ctx, token)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return user, nil
}

// Logout deletes the session row (best effort) and clears the cookie.
func (s *Service) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if token, ok := s.signer.Unseal(cookie.Value); ok {
			if err := s.repo.DeleteSession(ctx, token); err != nil {
				s.log.Error("logout: failed to delete session", "error", err)
			}
		}
	}
	s.ClearSessionCookie(w)
}

// ClearSessionCookie invalidates the session cookie on the client.
func (s *Service) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

type githubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

func fetchGitHubUser(ctx context.Context, conf *oauth2.Config, token *oauth2.Token) (*githubUser, error) {
	client := conf.Client(ctx, token)
	client.Timeout = githubUserTimeout

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubUserURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch github user: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github user endpoint returned status %d", resp.StatusCode)
	}

	var user githubUser
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(&user); err != nil {
		return nil, fmt.Errorf("decode github user: %w", err)
	}
	if user.ID == 0 || user.Login == "" {
		return nil, errors.New("github user response missing id or login")
	}
	return &user, nil
}

func randomHex(byteLen int) (string, error) {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return hex.EncodeToString(b), nil
}
