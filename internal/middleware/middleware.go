// Package middleware provides request logging, panic recovery, security
// headers, session resolution, and CSRF protection.
//
// CSRF uses the double-submit cookie pattern: a random token is stored in an
// HttpOnly cookie and embedded in every form as a hidden field; POST requests
// must present both and they must match (constant-time comparison). This is
// independent of (and in addition to) SameSite cookies.
package middleware

import (
    "context"
    "crypto/rand"
    "crypto/subtle"
    "encoding/hex"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "runtime/debug"
    "time"

    "markdown-social/internal/auth"
    "markdown-social/internal/models"
)

const (
    csrfCookieName = "ms_csrf"
    csrfFormField  = "csrf_token"
    csrfTokenBytes = 32
    csrfCookieTTL  = 12 * time.Hour
    maxBodyBytes   = 1 << 20
)

type ctxKey int

const (
    ctxKeyUser ctxKey = iota
    ctxKeyCSRFToken
)

// WithUser stores the resolved user (possibly nil) in the request context.
func WithUser(ctx context.Context, u *models.User) context.Context {
    return context.WithValue(ctx, ctxKeyUser, u)
}

// UserFrom returns the current user or nil.
func UserFrom(ctx context.Context) *models.User {
    u, _ := ctx.Value(ctxKeyUser).(*models.User)
    return u
}

// WithCSRFToken stores the CSRF token in the request context.
func WithCSRFToken(ctx context.Context, token string) context.Context {
    return context.WithValue(ctx, ctxKeyCSRFToken, token)
}

// CSRFTokenFrom returns the CSRF token for the current request.
func CSRFTokenFrom(ctx context.Context) string {
    t, _ := ctx.Value(ctxKeyCSRFToken).(string)
    return t
}

// Middleware composes the request pipeline.
type Middleware struct {
    log    *slog.Logger
    auth   *auth.Service
    secure bool
}

// New creates the middleware stack.
func New(log *slog.Logger, authSvc *auth.Service, secure bool) *Middleware {
    if log == nil {
        log = slog.Default()
    }
    return &Middleware{log: log, auth: authSvc, secure: secure}
}

// Wrap chains every middleware around next.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
    return m.Recover(m.RequestLog(m.SecurityHeaders(m.Session(m.CSRF(next)))))
}

// Recover turns handler panics into clean 500 responses without exposing
// stack traces to users.
func (m *Middleware) Recover(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            if rec := recover(); rec != nil {
                m.log.Error("panic recovered",
                    "method", r.Method, "path", r.URL.Path,
                    "panic", rec, "stack", string(debug.Stack()))
                w.WriteHeader(http.StatusInternalServerError)
                _, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Server error</title><link rel="stylesheet" href="/static/style.css"></head><body><main class="container error-page"><p class="error-status">500</p><h1>Something went wrong</h1><p>An unexpected error occurred. Please try again in a moment.</p><p><a class="button" href="/">Back to the feed</a></p></main></body></html>`)
            }
        }()
        next.ServeHTTP(w, r)
    })
}

type statusWriter struct {
    http.ResponseWriter
    status      int
    bytes       int
    wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
    if !sw.wroteHeader {
        sw.status = code
        sw.wroteHeader = true
    }
    sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
    if !sw.wroteHeader {
        sw.WriteHeader(http.StatusOK)
    }
    n, err := sw.ResponseWriter.Write(b)
    sw.bytes += n
    return n, err
}

// RequestLog logs each request (static assets at debug level).
func (m *Middleware) RequestLog(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
        next.ServeHTTP(sw, r)
        attrs := []any{"method", r.Method, "path", r.URL.Path, "status", sw.status, "bytes", sw.bytes, "duration", time.Since(start).String()}
        if isStaticPath(r.URL.Path) {
            m.log.Debug("request", attrs...)
        } else {
            m.log.Info("request", attrs...)
        }
    })
}

func isStaticPath(path string) bool {
    return len(path) >= 8 && path[:8] == "/static/"
}

// SecurityHeaders sets the HTTP security response headers. The CSP forbids
// inline script/style (the app ships only external, same-origin assets),
// forbids framing, and restricts images to https (GitHub avatars and
// user-posted images) plus same-origin.
func (m *Middleware) SecurityHeaders(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        h := w.Header()
        h.Set("Content-Security-Policy",
            "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' https:; "+
                "font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; "+
                "form-action 'self'; frame-ancestors 'none'")
        h.Set("X-Content-Type-Options", "nosniff")
        h.Set("X-Frame-Options", "DENY")
        h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
        h.Set("Permissions-Policy",
            "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), "+
                "microphone=(), payment=(), usb=(), interest-cohort=()")
        h.Set("Cache-Control", "private, no-store")
        next.ServeHTTP(w, r)
    })
}

// Session resolves the current user into the request context (nil for
// anonymous visitors). Database failures degrade to anonymous rather than
// crashing the request; mutations all require authentication anyway.
func (m *Middleware) Session(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var user *models.User
        if m.auth != nil {
            u, err := m.auth.CurrentUser(r.Context(), r)
            if err != nil {
                m.log.Error("session lookup failed", "path", r.URL.Path, "error", err)
            } else {
                user = u
            }
        }
        next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), user)))
    })
}

// CSRF issues tokens on safe requests and validates them on unsafe requests.
func (m *Middleware) CSRF(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        switch r.Method {
        case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
            token := ""
            if cookie, err := r.Cookie(csrfCookieName); err == nil && isHexToken(cookie.Value) {
                token = cookie.Value
            } else {
                var err error
                token, err = randomHex(csrfTokenBytes)
                if err != nil {
                    m.log.Error("csrf token generation failed", "error", err)
                    m.plainError(w, http.StatusInternalServerError)
                    return
                }
                http.SetCookie(w, &http.Cookie{
                    Name:     csrfCookieName,
                    Value:    token,
                    Path:     "/",
                    MaxAge:   int(csrfCookieTTL.Seconds()),
                    HttpOnly: true,
                    Secure:   m.secure,
                    SameSite: http.SameSiteLaxMode,
                })
            }
            next.ServeHTTP(w, r.WithContext(WithCSRFToken(r.Context(), token)))
            return
        default:
            cookie, err := r.Cookie(csrfCookieName)
            if err != nil || !isHexToken(cookie.Value) {
                m.log.Warn("csrf rejected: missing or malformed cookie", "method", r.Method, "path", r.URL.Path)
                m.csrfFailure(w)
                return
            }
            r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
            if err := r.ParseForm(); err != nil {
                m.log.Warn("csrf rejected: unparseable form", "method", r.Method, "path", r.URL.Path)
                m.plainError(w, http.StatusBadRequest)
                return
            }
            submitted := r.PostFormValue(csrfFormField)
            if len(submitted) != len(cookie.Value) ||
                subtle.ConstantTimeCompare([]byte(submitted), []byte(cookie.Value)) != 1 {
                m.log.Warn("csrf rejected: token mismatch", "method", r.Method, "path", r.URL.Path)
                m.csrfFailure(w)
                return
            }
            next.ServeHTTP(w, r.WithContext(WithCSRFToken(r.Context(), cookie.Value)))
        }
    })
}

func (m *Middleware) csrfFailure(w http.ResponseWriter) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.WriteHeader(http.StatusForbidden)
    _, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Request blocked</title><link rel="stylesheet" href="/static/style.css"></head><body><main class="container error-page"><p class="error-status">403</p><h1>Request blocked</h1><p>Your request could not be verified. Go back, reload the page, and try again.</p><p><a class="button" href="/">Back to the feed</a></p></main></body></html>`)
}

func (m *Middleware) plainError(w http.ResponseWriter, status int) {
    http.Error(w, http.StatusText(status), status)
}

func isHexToken(v string) bool {
    if len(v) != csrfTokenBytes*2 {
        return false
    }
    _, err := hex.DecodeString(v)
    return err == nil
}

func randomHex(byteLen int) (string, error) {
    b := make([]byte, byteLen)
    if _, err := rand.Read(b); err != nil {
        return "", fmt.Errorf("generate csrf token: %w", err)
    }
    return hex.EncodeToString(b), nil
}
