// Package handlers implements every HTTP handler for the application and the
// route table. Handlers parse and validate requests, enforce authentication
// and authorization, invoke the moderation and markdown services, call the
// repository, and render templates or redirect. Business rules live in the
// repository and service packages.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"markdown-social/internal/auth"
	"markdown-social/internal/config"
	"markdown-social/internal/markdown"
	"markdown-social/internal/middleware"
	"markdown-social/internal/models"
	"markdown-social/internal/moderation"
	"markdown-social/internal/repository"
)

const (
	feedPageSize      = 10
	maxTitleLen       = 120
	maxPostLen        = 20000
	maxCommentLen     = 4000
	maxBioLen         = 2000
	flashCookie       = "ms_flash"
	flashCookieTTL    = 60
	deleteConfirm     = "DELETE"
	moderationTimeout = 20 * time.Second
)

// Friendly messages shown to users; internal details are logged, never shown.
const (
	msgModerationDownPost = "We could not complete the safety check for your post. Nothing was saved — please try again in a few minutes."
	msgModerationDownBio  = "We could not complete the safety check for your bio. Nothing was saved — please try again in a few minutes."
	msgModerationDownComm = "We could not complete the safety check for your comment. Nothing was saved — please try again in a few minutes."
	msgModerationPost     = "Your post was flagged by the automated content check for harassment or abuse, so it was not published. Nothing was saved."
	msgModerationBio      = "Your bio was flagged by the automated content check for harassment or abuse, so it was not saved."
	msgModerationComment  = "Your comment was flagged by the automated content check for harassment or abuse, so it was not posted."
)

var pageNames = []string{
	"home", "login", "post", "post_new", "post_edit",
	"profile", "settings", "settings_profile", "error",
}

// Deps wires the handler with its collaborators.
type Deps struct {
	Repo        *repository.Repository
	Auth        *auth.Service
	Moderation  *moderation.Service
	Markdown    *markdown.Renderer
	Signer      *auth.Signer
	Config      *config.Config
	Log         *slog.Logger
	TemplateDir string
	StaticDir   string
}

// Handler serves every route.
type Handler struct {
	repo        *repository.Repository
	auth        *auth.Service
	moderation  *moderation.Service
	md          *markdown.Renderer
	signer      *auth.Signer
	cfg         *config.Config
	log         *slog.Logger
	pages       map[string]*template.Template
	templateDir string
	staticDir   string
}

// New validates dependencies and parses all templates.
func New(deps Deps) (*Handler, error) {
	if deps.Repo == nil || deps.Auth == nil || deps.Moderation == nil ||
		deps.Markdown == nil || deps.Signer == nil || deps.Config == nil || deps.Log == nil {
		return nil, errors.New("handlers: all dependencies are required")
	}
	templateDir := deps.TemplateDir
	if templateDir == "" {
		templateDir = "web/templates"
	}
	staticDir := deps.StaticDir
	if staticDir == "" {
		staticDir = "web/static"
	}
	h := &Handler{
		repo:        deps.Repo,
		auth:        deps.Auth,
		moderation:  deps.Moderation,
		md:          deps.Markdown,
		signer:      deps.Signer,
		cfg:         deps.Config,
		log:         deps.Log,
		pages:       make(map[string]*template.Template, len(pageNames)),
		templateDir: templateDir,
		staticDir:   staticDir,
	}
	if err := h.parseTemplates(); err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return h, nil
}

// parseTemplates builds one template set per page: the shared layout (with
// nav/footer partials) plus the page bodies defined in pages.html/forms.html,
// selected through a per-set "content" definition.
func (h *Handler) parseTemplates() error {
	funcs := template.FuncMap{
		"humantime": humanTime,
		"pluralize": pluralize,
		"initial":   initial,
	}
	layoutPath := filepath.Join(h.templateDir, "layout.html")
	contentPaths := []string{
		filepath.Join(h.templateDir, "pages.html"),
		filepath.Join(h.templateDir, "forms.html"),
	}

	base, err := template.New("layout.html").Funcs(funcs).ParseFiles(layoutPath)
	if err != nil {
		return err
	}

	for _, name := range pageNames {
		set, err := base.Clone()
		if err != nil {
			return err
		}
		glue := fmt.Sprintf(`{{define "content"}}{{template "page_%s" .}}{{end}}`, name)
		if _, err := set.Parse(glue); err != nil {
			return err
		}
		if _, err := set.ParseFiles(contentPaths...); err != nil {
			return err
		}
		tmpl := set.Lookup("layout")
		if tmpl == nil {
			return fmt.Errorf("layout template missing from %s", layoutPath)
		}
		h.pages[name] = tmpl
	}
	return nil
}

// Routes registers every route on a ServeMux using Go 1.22+ pattern routing.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", h.Home)
	mux.HandleFunc("GET /login", h.Login)
	mux.HandleFunc("GET /auth/github", h.GitHubLogin)
	mux.HandleFunc("GET /auth/github/callback", h.GitHubCallback)
	mux.HandleFunc("POST /logout", h.Logout)

	// Operational and crawler endpoints. These are deliberately cheap (no
	// session lookup, no CSRF cookie - see middleware.bypassesSessionAndCSRF)
	// so a platform health check or a bot never competes with real users for
	// a database connection, especially right after a cold start.
	mux.HandleFunc("GET /healthz", h.Healthz)
	mux.HandleFunc("GET /readyz", h.Readyz)
	mux.HandleFunc("GET /robots.txt", h.Robots)
	mux.HandleFunc("GET /sitemap.xml", h.Sitemap)
	mux.HandleFunc("GET /favicon.ico", h.Favicon)

	mux.HandleFunc("GET /posts/new", h.NewPostForm)
	mux.HandleFunc("POST /posts", h.CreatePost)
	mux.HandleFunc("GET /posts/{id}", h.ShowPost)
	mux.HandleFunc("GET /posts/{id}/edit", h.EditPostForm)
	mux.HandleFunc("POST /posts/{id}/edit", h.UpdatePost)
	mux.HandleFunc("POST /posts/{id}/delete", h.DeletePost)
	mux.HandleFunc("POST /posts/{id}/like", h.LikePost)
	mux.HandleFunc("POST /posts/{id}/unlike", h.UnlikePost)
	mux.HandleFunc("POST /posts/{id}/comments", h.CreateComment)
	mux.HandleFunc("POST /comments/{id}/delete", h.DeleteComment)

	mux.HandleFunc("GET /users/{username}", h.Profile)

	mux.HandleFunc("GET /settings", h.Settings)
	mux.HandleFunc("GET /settings/profile", h.SettingsProfile)
	mux.HandleFunc("POST /settings/profile", h.UpdateProfile)
	mux.HandleFunc("POST /settings/account/delete", h.DeleteAccount)

	static := http.StripPrefix("/static/", http.FileServer(http.Dir(h.staticDir)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		// Block directory listings, dotfiles, and any traversal-ish input.
		if name == "" || strings.HasPrefix(name, ".") || strings.Contains(name, "..") || strings.ContainsAny(name, "\\") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		static.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/", h.NotFound)
	return mux
}

// ---------------------------------------------------------------------------
// Health, readiness, and crawler endpoints
// ---------------------------------------------------------------------------

// readyzTimeout bounds the database ping so a slow/cold database cannot make
// the readiness check itself hang; a platform polling this endpoint should
// see a fast, definite answer either way.
const readyzTimeout = 3 * time.Second

// Healthz is a pure liveness check: it reports 200 the moment the process is
// up and able to handle a request, without touching the database or any
// external service. A platform's wake-up/health probe should prefer this
// over "/", which requires a database round trip (the feed query) and can
// therefore look "not ready" for longer than the process actually is.
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readyz additionally verifies the database is reachable, bounded by
// readyzTimeout. Use this (rather than "/") wherever a platform needs to
// confirm the application can actually serve real traffic, not just that the
// process has started.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := h.repo.Pool().Ping(ctx); err != nil {
		h.log.Warn("readyz: database ping failed", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("database unavailable"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Robots serves a small, accurate robots.txt: public pages are crawlable,
// authenticated-only and utility pages are not, and the sitemap is
// referenced so crawlers can discover posts directly.
func (h *Handler) Robots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	fmt.Fprintf(w, "User-agent: *\n"+
		"Allow: /\n"+
		"Disallow: /posts/new\n"+
		"Disallow: /*/edit\n"+
		"Disallow: /settings\n"+
		"Disallow: /login\n"+
		"Disallow: /auth/\n"+
		"Sitemap: %s/sitemap.xml\n", h.cfg.AppURL)
}

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

// sitemapMaxPosts bounds the sitemap to the most recently published posts so
// this handler stays a single, cheap, bounded query no matter how large the
// site grows; older posts remain reachable through the feed's pagination and
// through search engines following links from newer posts.
const sitemapMaxPosts = 1000

// Sitemap lists the public feed and every post (most recent first, capped at
// sitemapMaxPosts) so search engines can discover content without crawling
// paginated feed listings.
func (h *Handler) Sitemap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()

	posts, _, err := h.repo.ListPosts(ctx, 0, sitemapMaxPosts, 0)
	if err != nil {
		h.log.Error("sitemap: list posts failed", "error", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	set := sitemapURLSet{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	set.URLs = append(set.URLs, sitemapURL{Loc: h.cfg.AppURL + "/"})
	for _, p := range posts {
		set.URLs = append(set.URLs, sitemapURL{
			Loc:     fmt.Sprintf("%s/posts/%d", h.cfg.AppURL, p.ID),
			LastMod: p.UpdatedAt.UTC().Format("2006-01-02"),
		})
	}

	body, err := xml.MarshalIndent(set, "", "  ")
	if err != nil {
		h.log.Error("sitemap: encode failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=900")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// Favicon redirects the conventional /favicon.ico request to the real,
// versioned static asset so browsers and crawlers that request it directly
// (rather than reading the <link rel="icon"> in the page head) still get an
// icon instead of a 404.
func (h *Handler) Favicon(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/static/favicon.svg", http.StatusFound)
}

// ---------------------------------------------------------------------------
// View models
// ---------------------------------------------------------------------------

type baseData struct {
	Title       string
	User        *models.User
	LoggedIn    bool
	CSRFToken   string
	Flash       string
	FlashKind   string
	Description string
	Canonical   string
	NoIndex     bool
	// JSONLD is pre-encoded structured data for this page (a single
	// <script type="application/ld+json"> body), or empty when a page has
	// nothing worth describing structurally. Building it with jsonLD (which
	// escapes "<", ">", "&") keeps it safe to emit as raw HTML.
	JSONLD template.HTML
}

type paginationData struct {
	Page  int
	Total int
	Base  string
}

func (p paginationData) HasPrev() bool { return p.Page > 1 }
func (p paginationData) HasNext() bool { return p.Page < p.Total }
func (p paginationData) PrevPage() int { return p.Page - 1 }
func (p paginationData) NextPage() int { return p.Page + 1 }

type homeView struct {
	baseData
	Posts      []models.PostView
	Pagination paginationData
}

type postFormView struct {
	baseData
	PostID      int64
	FormTitle   string
	FormContent string
	FormError   string
}

type postView struct {
	baseData
	Post         *models.PostView
	Comments     []models.CommentView
	CommentDraft string
	CommentError string
}

type profileView struct {
	baseData
	Profile    *models.User
	IsOwner    bool
	Posts      []models.PostView
	Pagination paginationData
}

type settingsView struct {
	baseData
	FormError string
}

type bioFormView struct {
	baseData
	FormBio   string
	FormError string
}

type errorView struct {
	baseData
	Status  int
	Message string
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

// render executes a page template into a buffer first so a template failure
// never emits a half-written response.
func (h *Handler) render(w http.ResponseWriter, status int, page string, data any) {
	tmpl, ok := h.pages[page]
	if !ok {
		h.log.Error("unknown page template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		h.log.Error("template execution failed", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// defaultDescription is used by any page that does not set a more specific
// one (see newView).
const defaultDescription = "Inkwell is a small, Markdown-first social network: write posts and comments in Markdown, rendered and safety-checked on the server."

// newView builds the common page data, consuming any pending flash message.
// It defaults Description to defaultDescription and Canonical to this
// request's own path on the app's public URL; callers with something more
// specific to say (a post's own excerpt, a private/utility page that should
// not be indexed) overwrite those fields on the returned value before
// rendering.
//
// The canonical URL intentionally drops the query string: for the paginated
// feed and profile pages this makes page 2+ canonicalize to page 1 rather
// than being indexed as a near-duplicate listing, which is the usual advice
// for simple offset pagination.
func (h *Handler) newView(w http.ResponseWriter, r *http.Request, title string) baseData {
	user := middleware.UserFrom(r.Context())
	msg, kind := h.consumeFlash(w, r)
	return baseData{
		Title:       title,
		User:        user,
		LoggedIn:    user != nil,
		CSRFToken:   middleware.CSRFTokenFrom(r.Context()),
		Flash:       msg,
		FlashKind:   kind,
		Description: defaultDescription,
		Canonical:   h.cfg.AppURL + r.URL.Path,
	}
}

// jsonLD encodes v as a single <script type="application/ld+json"> element.
// json.Marshal never emits raw "<", ">", or "&" by default for string
// values containing them (it escapes to \u003c etc.) except inside already-
// encoded HTML fields, which none of our structured data includes; the
// extra replacement below is a defense-in-depth guard against "</script>"
// ever prematurely closing the tag, not a correctness requirement.
func jsonLD(v any) (template.HTML, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	safe := strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`).Replace(string(body))
	return template.HTML(`<script type="application/ld+json">` + safe + `</script>`), nil
}

// redirect issues a 303 See Other (the PRG status after a POST).
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, target string) {
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// setFlash stores a signed one-shot message cookie.
func (h *Handler) setFlash(w http.ResponseWriter, message, kind string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    h.signer.Seal(kind + "|" + message),
		Path:     "/",
		MaxAge:   flashCookieTTL,
		HttpOnly: true,
		Secure:   h.cfg.IsProduction(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) consumeFlash(w http.ResponseWriter, r *http.Request) (string, string) {
	cookie, err := r.Cookie(flashCookie)
	if err != nil || cookie.Value == "" {
		return "", ""
	}
	h.clearFlash(w)
	value, ok := h.signer.Unseal(cookie.Value)
	if !ok {
		return "", ""
	}
	parts := strings.SplitN(value, "|", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[1], parts[0]
}

func (h *Handler) clearFlash(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.IsProduction(), SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	data := h.newView(w, r, "Page not found")
	data.NoIndex = true
	h.render(w, http.StatusNotFound, "error", errorView{
		baseData: data,
		Status:   http.StatusNotFound,
		Message:  "The page you requested does not exist or may have been removed.",
	})
}

func (h *Handler) NotFound(w http.ResponseWriter, r *http.Request) {
	h.notFound(w, r)
}

func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request, message string) {
	data := h.newView(w, r, "Not allowed")
	data.NoIndex = true
	h.render(w, http.StatusForbidden, "error", errorView{
		baseData: data,
		Status:   http.StatusForbidden,
		Message:  message,
	})
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	// The underlying error (SQL, OAuth, moderation detail) is logged for
	// operators, never shown to users.
	h.log.Error("internal error", "method", r.Method, "path", r.URL.Path, "error", err)
	data := h.newView(w, r, "Something went wrong")
	data.NoIndex = true
	h.render(w, http.StatusInternalServerError, "error", errorView{
		baseData: data,
		Status:   http.StatusInternalServerError,
		Message:  "An unexpected error occurred. Please try again in a moment.",
	})
}

func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) *models.User {
	user := middleware.UserFrom(r.Context())
	if user == nil {
		h.redirect(w, r, "/login")
		return nil
	}
	return user
}

func (h *Handler) moderate(ctx context.Context, content string) (moderation.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, moderationTimeout)
	defer cancel()
	return h.moderation.Moderate(ctx, content)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func parsePage(r *http.Request) int {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		return 1
	}
	if page > 10000 {
		return 10000
	}
	return page
}

func totalPages(total int) int {
	pages := (total + feedPageSize - 1) / feedPageSize
	if pages < 1 {
		return 1
	}
	return pages
}

func validatePost(title, content string) string {
	if title == "" {
		return "A title is required."
	}
	if utf8.RuneCountInString(title) > maxTitleLen {
		return "Titles must be at most 120 characters."
	}
	if content == "" {
		return "Post content is required."
	}
	if utf8.RuneCountInString(content) > maxPostLen {
		return "Posts must be at most 20,000 characters."
	}
	return ""
}

// ---------------------------------------------------------------------------
// Public pages
// ---------------------------------------------------------------------------

func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.UserFrom(r.Context())
	viewerID := int64(0)
	if viewer != nil {
		viewerID = viewer.ID
	}

	page := parsePage(r)
	posts, total, err := h.repo.ListPosts(r.Context(), viewerID, feedPageSize, (page-1)*feedPageSize)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	csrf := middleware.CSRFTokenFrom(r.Context())
	for i := range posts {
		posts[i].CSRFToken = csrf
		posts[i].ViewerIsAuthor = viewer != nil && posts[i].AuthorID == viewer.ID
	}

	data := h.newView(w, r, "Feed")
	data.Canonical = h.cfg.AppURL + "/" // page 2+ still canonicalizes to the feed root
	if page == 1 {
		siteLD, err := jsonLD(map[string]any{
			"@context": "https://schema.org",
			"@type":    "WebSite",
			"name":     "Inkwell",
			"url":      h.cfg.AppURL + "/",
		})
		if err != nil {
			h.log.Error("home: encode structured data failed", "error", err)
		} else {
			data.JSONLD = siteLD
		}
	}
	h.render(w, http.StatusOK, "home", homeView{
		baseData:   data,
		Posts:      posts,
		Pagination: paginationData{Page: page, Total: totalPages(int(total)), Base: "/?"},
	})
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	if middleware.UserFrom(r.Context()) != nil {
		h.redirect(w, r, "/")
		return
	}
	data := h.newView(w, r, "Sign in")
	data.NoIndex = true
	h.render(w, http.StatusOK, "login", data)
}

func (h *Handler) GitHubLogin(w http.ResponseWriter, r *http.Request) {
	h.auth.BeginLogin(w, r)
}

func (h *Handler) GitHubCallback(w http.ResponseWriter, r *http.Request) {
	user, err := h.auth.CompleteLogin(r.Context(), w, r)
	if err != nil {
		// Details (state mismatch, token exchange failures, provider errors)
		// are logged server-side; the user only sees a friendly message.
		h.log.Error("github sign-in failed", "error", err)
		h.setFlash(w, "We could not complete sign-in with GitHub. Please try again.", "error")
		h.redirect(w, r, "/login")
		return
	}
	h.setFlash(w, "Welcome back, "+user.DisplayName+"!", "success")
	h.redirect(w, r, "/")
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	h.auth.Logout(r.Context(), w, r)
	h.setFlash(w, "You have been signed out.", "success")
	h.redirect(w, r, "/")
}

func (h *Handler) ShowPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	view := h.loadPostView(w, r, id)
	if view == nil {
		return
	}
	h.render(w, http.StatusOK, "post", *view)
}

// loadPostView assembles the post detail view (post + comments) or writes the
// appropriate error response and returns nil.
func (h *Handler) loadPostView(w http.ResponseWriter, r *http.Request, postID int64) *postView {
	viewer := middleware.UserFrom(r.Context())
	viewerID := int64(0)
	if viewer != nil {
		viewerID = viewer.ID
	}

	post, err := h.repo.GetPost(r.Context(), postID, viewerID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			h.notFound(w, r)
		} else {
			h.serverError(w, r, err)
		}
		return nil
	}
	comments, err := h.repo.ListComments(r.Context(), postID)
	if err != nil {
		h.serverError(w, r, err)
		return nil
	}

	csrf := middleware.CSRFTokenFrom(r.Context())
	post.CSRFToken = csrf
	post.ViewerIsAuthor = viewer != nil && post.AuthorID == viewer.ID
	for i := range comments {
		comments[i].CSRFToken = csrf
		comments[i].ViewerIsAuthor = viewer != nil && comments[i].AuthorID == viewer.ID
	}

	data := h.newView(w, r, post.Title)
	data.Description = excerpt(post.ContentMarkdown, 160)
	postLD, err := jsonLD(map[string]any{
		"@context":      "https://schema.org",
		"@type":         "BlogPosting",
		"headline":      post.Title,
		"datePublished": post.CreatedAt.UTC().Format(time.RFC3339),
		"dateModified":  post.UpdatedAt.UTC().Format(time.RFC3339),
		"author": map[string]any{
			"@type": "Person",
			"name":  post.AuthorDisplayName,
			"url":   h.cfg.AppURL + "/users/" + post.AuthorUsername,
		},
		"mainEntityOfPage": data.Canonical,
	})
	if err != nil {
		h.log.Error("post: encode structured data failed", "error", err)
	} else {
		data.JSONLD = postLD
	}

	return &postView{
		baseData: data,
		Post:     post,
		Comments: comments,
	}
}

// excerpt turns Markdown source into a short, plain-text summary suitable
// for a meta description: it strips the most common Markdown syntax
// characters, collapses whitespace, and truncates on a word boundary. It is
// intentionally approximate (this is metadata for search results, not
// rendered content) - the server's real Markdown renderer is what produces
// the actual page.
func excerpt(markdownSource string, maxLen int) string {
	replacer := strings.NewReplacer(
		"#", "", "*", "", "_", "", "`", "", ">", "", "~", "",
		"\r\n", " ", "\n", " ", "\t", " ",
	)
	text := strings.Join(strings.Fields(replacer.Replace(markdownSource)), " ")
	if text == "" {
		return defaultDescription
	}
	if utf8.RuneCountInString(text) <= maxLen {
		return text
	}
	runes := []rune(text)
	cut := runes[:maxLen]
	if i := strings.LastIndexByte(string(cut), ' '); i > 0 {
		cut = []rune(string(cut)[:i])
	}
	return strings.TrimRight(string(cut), ".,;:!?") + "\u2026"
}

func (h *Handler) Profile(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	viewer := middleware.UserFrom(r.Context())
	viewerID := int64(0)
	if viewer != nil {
		viewerID = viewer.ID
	}

	profile, err := h.repo.GetUserByUsername(r.Context(), username)
	if errors.Is(err, repository.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	page := parsePage(r)
	posts, total, err := h.repo.ListPostsByAuthor(r.Context(), profile.ID, viewerID, feedPageSize, (page-1)*feedPageSize)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	csrf := middleware.CSRFTokenFrom(r.Context())
	for i := range posts {
		posts[i].CSRFToken = csrf
		posts[i].ViewerIsAuthor = viewer != nil && posts[i].AuthorID == viewer.ID
	}

	data := h.newView(w, r, profile.DisplayName)
	data.Description = fmt.Sprintf("Posts by %s (@%s) on Inkwell.", profile.DisplayName, profile.Username)
	data.Canonical = h.cfg.AppURL + "/users/" + url.PathEscape(profile.Username)
	h.render(w, http.StatusOK, "profile", profileView{
		baseData: data,
		Profile:  profile,
		IsOwner:  viewer != nil && viewer.ID == profile.ID,
		Posts:    posts,
		Pagination: paginationData{
			Page:  page,
			Total: totalPages(int(total)),
			Base:  "/users/" + url.PathEscape(profile.Username) + "?",
		},
	})
}

// ---------------------------------------------------------------------------
// Posts
// ---------------------------------------------------------------------------

func (h *Handler) NewPostForm(w http.ResponseWriter, r *http.Request) {
	if h.requireUser(w, r) == nil {
		return
	}
	h.render(w, http.StatusOK, "post_new", postFormView{baseData: h.newView(w, r, "New post")})
}

func (h *Handler) CreatePost(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}

	title := strings.TrimSpace(r.PostFormValue("title"))
	content := strings.TrimSpace(r.PostFormValue("content"))
	form := postFormView{baseData: h.newView(w, r, "New post"), FormTitle: title, FormContent: content}

	if msg := validatePost(title, content); msg != "" {
		form.FormError = msg
		h.render(w, http.StatusBadRequest, "post_new", form)
		return
	}

	result, err := h.moderate(r.Context(), content)
	if err != nil {
		// Fail closed: nothing is stored when the safety check cannot run.
		h.log.Error("moderation check failed for post", "user_id", user.ID, "error", err)
		form.FormError = msgModerationDownPost
		h.render(w, http.StatusServiceUnavailable, "post_new", form)
		return
	}
	if result.Decision == moderation.DecisionReject {
		h.log.Info("post rejected by moderation", "user_id", user.ID, "confidence", result.Confidence)
		form.FormError = msgModerationPost
		h.render(w, http.StatusBadRequest, "post_new", form)
		return
	}

	html, err := h.md.Render(content)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	postID, err := h.repo.CreatePost(r.Context(), user.ID, title, content, string(html))
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	h.setFlash(w, "Post published.", "success")
	h.redirect(w, r, fmt.Sprintf("/posts/%d", postID))
}

func (h *Handler) EditPostForm(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}

	post, err := h.repo.GetPost(r.Context(), id, user.ID)
	if errors.Is(err, repository.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if post.AuthorID != user.ID {
		h.forbidden(w, r, "You can only edit your own posts.")
		return
	}

	h.render(w, http.StatusOK, "post_edit", postFormView{
		baseData:    h.newView(w, r, "Edit post"),
		PostID:      id,
		FormTitle:   post.Title,
		FormContent: post.ContentMarkdown,
	})
}

func (h *Handler) UpdatePost(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}

	post, err := h.repo.GetPost(r.Context(), id, user.ID)
	if errors.Is(err, repository.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if post.AuthorID != user.ID {
		h.forbidden(w, r, "You can only edit your own posts.")
		return
	}

	title := strings.TrimSpace(r.PostFormValue("title"))
	content := strings.TrimSpace(r.PostFormValue("content"))
	form := postFormView{baseData: h.newView(w, r, "Edit post"), PostID: id, FormTitle: title, FormContent: content}

	if msg := validatePost(title, content); msg != "" {
		form.FormError = msg
		h.render(w, http.StatusBadRequest, "post_edit", form)
		return
	}

	// Unchanged content: skip moderation (no unnecessary Mistral calls) and
	// skip the write entirely.
	if title == post.Title && content == post.ContentMarkdown {
		h.redirect(w, r, fmt.Sprintf("/posts/%d", id))
		return
	}

	result, err := h.moderate(r.Context(), content)
	if err != nil {
		h.log.Error("moderation check failed for post edit", "user_id", user.ID, "post_id", id, "error", err)
		form.FormError = msgModerationDownPost
		h.render(w, http.StatusServiceUnavailable, "post_edit", form)
		return
	}
	if result.Decision == moderation.DecisionReject {
		h.log.Info("post edit rejected by moderation", "user_id", user.ID, "post_id", id, "confidence", result.Confidence)
		form.FormError = msgModerationPost
		h.render(w, http.StatusBadRequest, "post_edit", form)
		return
	}

	html, err := h.md.Render(content)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	updated, err := h.repo.UpdatePost(r.Context(), id, user.ID, title, content, string(html))
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if !updated {
		h.notFound(w, r)
		return
	}

	h.setFlash(w, "Post updated.", "success")
	h.redirect(w, r, fmt.Sprintf("/posts/%d", id))
}

func (h *Handler) DeletePost(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}

	post, err := h.repo.GetPost(r.Context(), id, user.ID)
	if errors.Is(err, repository.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if post.AuthorID != user.ID {
		h.forbidden(w, r, "You can only delete your own posts.")
		return
	}

	deleted, err := h.repo.DeletePost(r.Context(), id, user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if !deleted {
		h.notFound(w, r)
		return
	}

	h.setFlash(w, "Post deleted.", "success")
	h.redirect(w, r, "/")
}

// ---------------------------------------------------------------------------
// Likes
// ---------------------------------------------------------------------------

func (h *Handler) LikePost(w http.ResponseWriter, r *http.Request) {
	h.setLike(w, r, true)
}

func (h *Handler) UnlikePost(w http.ResponseWriter, r *http.Request) {
	h.setLike(w, r, false)
}

func (h *Handler) setLike(w http.ResponseWriter, r *http.Request, like bool) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}

	exists, err := h.repo.PostExists(r.Context(), id)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if !exists {
		h.notFound(w, r)
		return
	}

	if like {
		if err := h.repo.LikePost(r.Context(), user.ID, id); err != nil {
			h.serverError(w, r, err)
			return
		}
	} else {
		if err := h.repo.UnlikePost(r.Context(), user.ID, id); err != nil {
			h.serverError(w, r, err)
			return
		}
	}
	h.redirect(w, r, fmt.Sprintf("/posts/%d", id))
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

func (h *Handler) CreateComment(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	postID, ok := pathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}

	exists, err := h.repo.PostExists(r.Context(), postID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if !exists {
		h.notFound(w, r)
		return
	}

	content := strings.TrimSpace(r.PostFormValue("content"))

	// fail re-renders the post page with the draft preserved so the author
	// does not lose their comment text.
	fail := func(status int, message string) {
		view := h.loadPostView(w, r, postID)
		if view == nil {
			return
		}
		view.CommentDraft = content
		view.CommentError = message
		h.render(w, status, "post", *view)
	}

	if content == "" {
		fail(http.StatusBadRequest, "A comment cannot be empty.")
		return
	}
	if utf8.RuneCountInString(content) > maxCommentLen {
		fail(http.StatusBadRequest, "Comments must be at most 4,000 characters.")
		return
	}

	result, err := h.moderate(r.Context(), content)
	if err != nil {
		h.log.Error("moderation check failed for comment", "user_id", user.ID, "post_id", postID, "error", err)
		fail(http.StatusServiceUnavailable, msgModerationDownComm)
		return
	}
	if result.Decision == moderation.DecisionReject {
		h.log.Info("comment rejected by moderation", "user_id", user.ID, "post_id", postID, "confidence", result.Confidence)
		fail(http.StatusBadRequest, msgModerationComment)
		return
	}

	html, err := h.md.Render(content)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	commentID, err := h.repo.CreateComment(r.Context(), postID, user.ID, content, string(html))
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	h.setFlash(w, "Comment posted.", "success")
	h.redirect(w, r, fmt.Sprintf("/posts/%d#comment-%d", postID, commentID))
}

func (h *Handler) DeleteComment(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	commentID, ok := pathID(r)
	if !ok {
		h.notFound(w, r)
		return
	}

	ref, err := h.repo.GetComment(r.Context(), commentID)
	if errors.Is(err, repository.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if ref.AuthorID != user.ID {
		h.forbidden(w, r, "You can only delete your own comments.")
		return
	}

	deleted, err := h.repo.DeleteComment(r.Context(), commentID, user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if !deleted {
		h.notFound(w, r)
		return
	}

	h.setFlash(w, "Comment deleted.", "success")
	h.redirect(w, r, fmt.Sprintf("/posts/%d", ref.PostID))
}

// ---------------------------------------------------------------------------
// Settings and account management
// ---------------------------------------------------------------------------

func (h *Handler) Settings(w http.ResponseWriter, r *http.Request) {
	if h.requireUser(w, r) == nil {
		return
	}
	h.render(w, http.StatusOK, "settings", settingsView{baseData: h.newView(w, r, "Settings")})
}

func (h *Handler) SettingsProfile(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}
	h.render(w, http.StatusOK, "settings_profile", bioFormView{
		baseData: h.newView(w, r, "Edit profile"),
		FormBio:  user.BioMarkdown,
	})
}

func (h *Handler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}

	bio := strings.TrimSpace(r.PostFormValue("bio"))
	form := bioFormView{baseData: h.newView(w, r, "Edit profile"), FormBio: bio}

	if utf8.RuneCountInString(bio) > maxBioLen {
		form.FormError = "Your bio must be at most 2,000 characters."
		h.render(w, http.StatusBadRequest, "settings_profile", form)
		return
	}

	// Unchanged bio: skip moderation and the write.
	if bio == user.BioMarkdown {
		h.redirect(w, r, "/users/"+url.PathEscape(user.Username))
		return
	}

	if bio != "" {
		result, err := h.moderate(r.Context(), bio)
		if err != nil {
			h.log.Error("moderation check failed for bio", "user_id", user.ID, "error", err)
			form.FormError = msgModerationDownBio
			h.render(w, http.StatusServiceUnavailable, "settings_profile", form)
			return
		}
		if result.Decision == moderation.DecisionReject {
			h.log.Info("bio rejected by moderation", "user_id", user.ID, "confidence", result.Confidence)
			form.FormError = msgModerationBio
			h.render(w, http.StatusBadRequest, "settings_profile", form)
			return
		}
	}

	html, err := h.md.Render(bio)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	if err := h.repo.UpdateBio(r.Context(), user.ID, bio, string(html)); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			h.notFound(w, r)
		} else {
			h.serverError(w, r, err)
		}
		return
	}

	h.setFlash(w, "Profile updated.", "success")
	h.redirect(w, r, "/users/"+url.PathEscape(user.Username))
}

func (h *Handler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	user := h.requireUser(w, r)
	if user == nil {
		return
	}

	// Deliberate confirmation: the user must type DELETE. Deletion can never
	// happen through a GET or an accidental POST.
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	if !strings.EqualFold(confirm, deleteConfirm) {
		h.render(w, http.StatusBadRequest, "settings", settingsView{
			baseData:  h.newView(w, r, "Settings"),
			FormError: "Type DELETE in the confirmation field to delete your account.",
		})
		return
	}

	// The repository removes the user, posts, comments, likes, sessions, and
	// OAuth records in one transaction (with schema-level cascades as a
	// backstop) or leaves everything untouched.
	if err := h.repo.DeleteAccount(r.Context(), user.ID); err != nil {
		h.log.Error("account deletion failed", "user_id", user.ID, "error", err)
		h.serverError(w, r, err)
		return
	}

	// The transaction removed the session row; clear the client cookie too.
	h.auth.ClearSessionCookie(w)
	h.setFlash(w, "Your account has been deleted.", "success")
	h.redirect(w, r, "/")
}

// ---------------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------------

func humanTime(t time.Time) string {
	return t.UTC().Format("Jan 2, 2006 · 3:04 PM")
}

func pluralize(n int64, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func initial(name string) string {
	for _, r := range name {
		return strings.ToUpper(string(r))
	}
	return "?"
}
