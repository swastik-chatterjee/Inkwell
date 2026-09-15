// Package repository implements all PostgreSQL persistence for Inkwell.
//
// Every query is parameterized ($1, $2, ...). User input is never concatenated
// into SQL. Account deletion runs in an explicit transaction and leaves no
// orphaned rows.
package repository

import (
    "context"
    "crypto/rand"
    "crypto/sha256"
    "encoding/hex"
    "errors"
    "fmt"
    "strconv"
    "strings"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "html/template"

    "markdown-social/internal/models"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("record not found")

const sessionTTL = 30 * 24 * time.Hour

// Repository wraps the connection pool.
type Repository struct {
    pool *pgxpool.Pool
}

// New creates a repository.
func New(pool *pgxpool.Pool) *Repository {
    return &Repository{pool: pool}
}

// Pool exposes the underlying pool (used by tests for direct assertions).
func (r *Repository) Pool() *pgxpool.Pool {
    return r.pool
}

const userCols = "id, username, display_name, avatar_url, github_login, bio_markdown, bio_html, created_at, updated_at"

const userColsJoined = "u.id, u.username, u.display_name, u.avatar_url, u.github_login, u.bio_markdown, u.bio_html, u.created_at, u.updated_at"

func scanUser(row pgx.Row) (*models.User, error) {
    var u models.User
    var bioHTML string
    err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.AvatarURL, &u.GitHubLogin, &u.BioMarkdown, &bioHTML, &u.CreatedAt, &u.UpdatedAt)
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, ErrNotFound
        }
        return nil, err
    }
    u.BioHTML = template.HTML(bioHTML)
    return &u, nil
}

// ---------------------------------------------------------------------------
// Users and OAuth accounts
// ---------------------------------------------------------------------------

// UpsertGitHubUser links a GitHub account to a local user. If the account is
// new, a user is created with a unique username derived from the GitHub login;
// otherwise the stored profile is refreshed from GitHub. Username collisions
// are resolved deterministically (login, login-2, login-3, ...).
func (r *Repository) UpsertGitHubUser(ctx context.Context, githubLogin string, githubID int64, displayName, avatarURL string) (*models.User, error) {
    displayName = truncateRunes(displayName, 120)
    avatarURL = truncateRunes(avatarURL, 1000)
    accountID := strconv.FormatInt(githubID, 10)
    provider := "github"

    tx, err := r.pool.Begin(ctx)
    if err != nil {
        return nil, fmt.Errorf("begin: %w", err)
    }
    defer tx.Rollback(ctx) //nolint:errcheck // rollback on commit is a no-op

    user, err := scanUser(tx.QueryRow(ctx, `
        SELECT `+userColsJoined+`
        FROM oauth_accounts oa
        JOIN users u ON u.id = oa.user_id
        WHERE oa.provider = $1 AND oa.provider_account_id = $2`,
        provider, accountID))

    if errors.Is(err, ErrNotFound) {
        if displayName == "" {
            displayName = githubLogin
        }
        username, err := uniqueUsername(ctx, tx, githubLogin)
        if err != nil {
            return nil, err
        }
        user, err = scanUser(tx.QueryRow(ctx, `
            INSERT INTO users (username, display_name, avatar_url, github_login)
            VALUES ($1, $2, $3, $4)
            RETURNING `+userCols,
            username, displayName, avatarURL, truncateRunes(githubLogin, 80)))
        if err != nil {
            return nil, fmt.Errorf("insert user: %w", err)
        }
        if _, err := tx.Exec(ctx, `
            INSERT INTO oauth_accounts (user_id, provider, provider_account_id)
            VALUES ($1, $2, $3)`,
            user.ID, provider, accountID); err != nil {
            return nil, fmt.Errorf("insert oauth account: %w", err)
        }
        if err := tx.Commit(ctx); err != nil {
            return nil, fmt.Errorf("commit: %w", err)
        }
        return user, nil
    }
    if err != nil {
        return nil, fmt.Errorf("lookup oauth account: %w", err)
    }

    // Existing account: GitHub is the source of truth for the profile.
    if displayName == "" {
        displayName = user.Username
    }
    if avatarURL == "" {
        avatarURL = user.AvatarURL
    }
    newUsername := user.Username
    if githubLogin != "" && githubLogin != user.Username {
        free, err := usernameAvailable(ctx, tx, sanitizeUsername(githubLogin))
        if err != nil {
            return nil, err
        }
        if free {
            newUsername = sanitizeUsername(githubLogin)
        }
    }
    user, err = scanUser(tx.QueryRow(ctx, `
        UPDATE users
        SET username = $2, display_name = $3, avatar_url = $4, github_login = $5, updated_at = now()
        WHERE id = $1
        RETURNING `+userCols,
        user.ID, newUsername, displayName, avatarURL, truncateRunes(githubLogin, 80)))
    if err != nil {
        return nil, fmt.Errorf("update user: %w", err)
    }
    if err := tx.Commit(ctx); err != nil {
        return nil, fmt.Errorf("commit: %w", err)
    }
    return user, nil
}

// GetUserByID fetches a user by primary key.
func (r *Repository) GetUserByID(ctx context.Context, id int64) (*models.User, error) {
    return scanUser(r.pool.QueryRow(ctx, `SELECT `+userColsJoined+` FROM users u WHERE u.id = $1`, id))
}

// GetUserByUsername fetches a user by username (profile URLs).
func (r *Repository) GetUserByUsername(ctx context.Context, username string) (*models.User, error) {
    return scanUser(r.pool.QueryRow(ctx, `SELECT `+userColsJoined+` FROM users u WHERE u.username = $1`, username))
}

// UpdateBio stores a new (already moderated and sanitized) bio.
func (r *Repository) UpdateBio(ctx context.Context, userID int64, bioMarkdown, bioHTML string) error {
    tag, err := r.pool.Exec(ctx, `
        UPDATE users SET bio_markdown = $2, bio_html = $3, updated_at = now()
        WHERE id = $1`,
        userID, bioMarkdown, bioHTML)
    if err != nil {
        return fmt.Errorf("update bio: %w", err)
    }
    if tag.RowsAffected() == 0 {
        return ErrNotFound
    }
    return nil
}

func uniqueUsername(ctx context.Context, tx pgx.Tx, login string) (string, error) {
    base := sanitizeUsername(login)
    if base == "" {
        base = "user"
    }
    candidate := base
    for i := 2; i < 100; i++ {
        free, err := usernameAvailable(ctx, tx, candidate)
        if err != nil {
            return "", err
        }
        if free {
            return candidate, nil
        }
        candidate = fmt.Sprintf("%s-%d", base, i)
    }
    // Astronomically unlikely: 99 colliding names. Add randomness.
    suffix, err := randomHex(4)
    if err != nil {
        return "", err
    }
    return base + "-" + suffix, nil
}

func usernameAvailable(ctx context.Context, tx pgx.Tx, username string) (bool, error) {
    var exists bool
    err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, username).Scan(&exists)
    if err != nil {
        return false, fmt.Errorf("check username: %w", err)
    }
    return !exists, nil
}

func sanitizeUsername(login string) string {
    login = strings.ToLower(strings.TrimSpace(login))
    var b strings.Builder
    lastDash := false
    for _, rn := range login {
        switch {
        case rn >= 'a' && rn <= 'z', rn >= '0' && rn <= '9':
            b.WriteRune(rn)
            lastDash = false
        default:
            if !lastDash {
                b.WriteByte('-')
                lastDash = true
            }
        }
    }
    name := strings.Trim(b.String(), "-")
    if len(name) > 39 {
        name = name[:39]
    }
    return strings.Trim(name, "-")
}

func truncateRunes(s string, limit int) string {
    if limit <= 0 {
        return ""
    }
    runes := []rune(s)
    if len(runes) <= limit {
        return s
    }
    return string(runes[:limit])
}

// ---------------------------------------------------------------------------
// Posts
// ---------------------------------------------------------------------------

const postSelect = `
    SELECT p.id, p.author_id, p.title, p.content_markdown, p.content_html,
           p.created_at, p.updated_at,
           u.username, u.display_name, u.avatar_url,
           (SELECT count(*) FROM likes l WHERE l.post_id = p.id) AS like_count,
           (SELECT count(*) FROM comments c WHERE c.post_id = p.id) AS comment_count,
           EXISTS (SELECT 1 FROM likes l WHERE l.post_id = p.id AND l.user_id = $1) AS liked_by_viewer
    FROM posts p
    JOIN users u ON u.id = p.author_id`

func scanPost(row pgx.Row) (*models.PostView, error) {
    var v models.PostView
    var contentHTML string
    err := row.Scan(&v.ID, &v.AuthorID, &v.Title, &v.ContentMarkdown, &contentHTML,
        &v.CreatedAt, &v.UpdatedAt,
        &v.AuthorUsername, &v.AuthorDisplayName, &v.AuthorAvatarURL,
        &v.LikeCount, &v.CommentCount, &v.LikedByViewer)
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, ErrNotFound
        }
        return nil, err
    }
    v.ContentHTML = template.HTML(contentHTML)
    return &v, nil
}

// GetPost fetches a single post for a viewer (viewerID 0 = anonymous).
func (r *Repository) GetPost(ctx context.Context, id, viewerID int64) (*models.PostView, error) {
    return scanPost(r.pool.QueryRow(ctx, postSelect+` WHERE p.id = $2`, viewerID, id))
}

// ListPosts returns one page of the public feed plus the total post count.
func (r *Repository) ListPosts(ctx context.Context, viewerID int64, limit, offset int) ([]models.PostView, int64, error) {
    rows, err := r.pool.Query(ctx, postSelect+`
        ORDER BY p.created_at DESC, p.id DESC
        LIMIT $2 OFFSET $3`, viewerID, limit, offset)
    if err != nil {
        return nil, 0, fmt.Errorf("list posts: %w", err)
    }
    defer rows.Close()

    posts := make([]models.PostView, 0, limit)
    for rows.Next() {
        v, err := scanPost(rows)
        if err != nil {
            return nil, 0, fmt.Errorf("scan post: %w", err)
        }
        posts = append(posts, *v)
    }
    if err := rows.Err(); err != nil {
        return nil, 0, fmt.Errorf("list posts: %w", err)
    }

    var total int64
    if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM posts`).Scan(&total); err != nil {
        return nil, 0, fmt.Errorf("count posts: %w", err)
    }
    return posts, total, nil
}

// ListPostsByAuthor returns one page of a user's posts plus their total count.
func (r *Repository) ListPostsByAuthor(ctx context.Context, authorID, viewerID int64, limit, offset int) ([]models.PostView, int64, error) {
    rows, err := r.pool.Query(ctx, postSelect+`
        WHERE p.author_id = $2
        ORDER BY p.created_at DESC, p.id DESC
        LIMIT $3 OFFSET $4`, viewerID, authorID, limit, offset)
    if err != nil {
        return nil, 0, fmt.Errorf("list posts by author: %w", err)
    }
    defer rows.Close()

    posts := make([]models.PostView, 0, limit)
    for rows.Next() {
        v, err := scanPost(rows)
        if err != nil {
            return nil, 0, fmt.Errorf("scan post: %w", err)
        }
        posts = append(posts, *v)
    }
    if err := rows.Err(); err != nil {
        return nil, 0, fmt.Errorf("list posts by author: %w", err)
    }

    var total int64
    if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE author_id = $1`, authorID).Scan(&total); err != nil {
        return nil, 0, fmt.Errorf("count posts by author: %w", err)
    }
    return posts, total, nil
}

// PostExists reports whether the post exists.
func (r *Repository) PostExists(ctx context.Context, id int64) (bool, error) {
    var exists bool
    err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1)`, id).Scan(&exists)
    if err != nil {
        return false, fmt.Errorf("check post: %w", err)
    }
    return exists, nil
}

// CreatePost stores a new (already moderated) post and returns its ID.
func (r *Repository) CreatePost(ctx context.Context, authorID int64, title, contentMarkdown, contentHTML string) (int64, error) {
    var id int64
    err := r.pool.QueryRow(ctx, `
        INSERT INTO posts (author_id, title, content_markdown, content_html)
        VALUES ($1, $2, $3, $4)
        RETURNING id`,
        authorID, title, contentMarkdown, contentHTML).Scan(&id)
    if err != nil {
        return 0, fmt.Errorf("create post: %w", err)
    }
    return id, nil
}

// UpdatePost updates a post only when it belongs to the given author. It
// returns false when the post does not exist or is not the author's.
func (r *Repository) UpdatePost(ctx context.Context, id, authorID int64, title, contentMarkdown, contentHTML string) (bool, error) {
    tag, err := r.pool.Exec(ctx, `
        UPDATE posts
        SET title = $3, content_markdown = $4, content_html = $5, updated_at = now()
        WHERE id = $1 AND author_id = $2`,
        id, authorID, title, contentMarkdown, contentHTML)
    if err != nil {
        return false, fmt.Errorf("update post: %w", err)
    }
    return tag.RowsAffected() == 1, nil
}

// DeletePost deletes a post only when it belongs to the given author.
func (r *Repository) DeletePost(ctx context.Context, id, authorID int64) (bool, error) {
    tag, err := r.pool.Exec(ctx, `DELETE FROM posts WHERE id = $1 AND author_id = $2`, id, authorID)
    if err != nil {
        return false, fmt.Errorf("delete post: %w", err)
    }
    return tag.RowsAffected() == 1, nil
}

// ---------------------------------------------------------------------------
// Comments
// ---------------------------------------------------------------------------

// CreateComment stores a new (already moderated) comment and returns its ID.
func (r *Repository) CreateComment(ctx context.Context, postID, authorID int64, contentMarkdown, contentHTML string) (int64, error) {
    var id int64
    err := r.pool.QueryRow(ctx, `
        INSERT INTO comments (post_id, author_id, content_markdown, content_html)
        VALUES ($1, $2, $3, $4)
        RETURNING id`,
        postID, authorID, contentMarkdown, contentHTML).Scan(&id)
    if err != nil {
        return 0, fmt.Errorf("create comment: %w", err)
    }
    return id, nil
}

// ListComments returns all comments on a post in chronological order.
func (r *Repository) ListComments(ctx context.Context, postID int64) ([]models.CommentView, error) {
    rows, err := r.pool.Query(ctx, `
        SELECT c.id, c.post_id, c.author_id, c.content_html, c.created_at,
               u.username, u.display_name, u.avatar_url
        FROM comments c
        JOIN users u ON u.id = c.author_id
        WHERE c.post_id = $1
        ORDER BY c.created_at ASC, c.id ASC`,
        postID)
    if err != nil {
        return nil, fmt.Errorf("list comments: %w", err)
    }
    defer rows.Close()

    comments := make([]models.CommentView, 0)
    for rows.Next() {
        var cv models.CommentView
        var contentHTML string
        if err := rows.Scan(&cv.ID, &cv.PostID, &cv.AuthorID, &contentHTML, &cv.CreatedAt,
            &cv.AuthorUsername, &cv.AuthorDisplayName, &cv.AuthorAvatarURL); err != nil {
            return nil, fmt.Errorf("scan comment: %w", err)
        }
        cv.ContentHTML = template.HTML(contentHTML)
        comments = append(comments, cv)
    }
    if err := rows.Err(); err != nil {
        return nil, fmt.Errorf("list comments: %w", err)
    }
    return comments, nil
}

// GetComment fetches a comment reference for authorization checks.
func (r *Repository) GetComment(ctx context.Context, id int64) (*models.CommentRef, error) {
    var ref models.CommentRef
    err := r.pool.QueryRow(ctx, `
        SELECT id, post_id, author_id FROM comments WHERE id = $1`, id).
        Scan(&ref.ID, &ref.PostID, &ref.AuthorID)
    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, ErrNotFound
        }
        return nil, fmt.Errorf("get comment: %w", err)
    }
    return &ref, nil
}

// DeleteComment deletes a comment only when it belongs to the given author.
func (r *Repository) DeleteComment(ctx context.Context, id, authorID int64) (bool, error) {
    tag, err := r.pool.Exec(ctx, `DELETE FROM comments WHERE id = $1 AND author_id = $2`, id, authorID)
    if err != nil {
        return false, fmt.Errorf("delete comment: %w", err)
    }
    return tag.RowsAffected() == 1, nil
}

// ---------------------------------------------------------------------------
// Likes
// ---------------------------------------------------------------------------

// LikePost records a like. The (user_id, post_id) primary key plus
// ON CONFLICT DO NOTHING makes this idempotent and safe under concurrency.
func (r *Repository) LikePost(ctx context.Context, userID, postID int64) error {
    _, err := r.pool.Exec(ctx, `
        INSERT INTO likes (user_id, post_id) VALUES ($1, $2)
        ON CONFLICT (user_id, post_id) DO NOTHING`,
        userID, postID)
    if err != nil {
        return fmt.Errorf("like post: %w", err)
    }
    return nil
}

// UnlikePost removes a like if present.
func (r *Repository) UnlikePost(ctx context.Context, userID, postID int64) error {
    _, err := r.pool.Exec(ctx, `DELETE FROM likes WHERE user_id = $1 AND post_id = $2`, userID, postID)
    if err != nil {
        return fmt.Errorf("unlike post: %w", err)
    }
    return nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSession issues a new opaque session token for a user. Only the
// SHA-256 hash of the token is stored; the raw token lives solely in the
// user's cookie.
func (r *Repository) CreateSession(ctx context.Context, userID int64) (string, time.Time, error) {
    token, err := randomHex(32)
    if err != nil {
        return "", time.Time{}, err
    }
    expires := time.Now().Add(sessionTTL)
    _, err = r.pool.Exec(ctx, `
        INSERT INTO sessions (token_hash, user_id, expires_at)
        VALUES ($1, $2, $3)`,
        hashToken(token), userID, expires)
    if err != nil {
        return "", time.Time{}, fmt.Errorf("create session: %w", err)
    }
    return token, expires, nil
}

// UserForSession resolves a session token to its user, ignoring expired
// sessions.
func (r *Repository) UserForSession(ctx context.Context, token string) (*models.User, error) {
    return scanUser(r.pool.QueryRow(ctx, `
        SELECT `+userColsJoined+`
        FROM sessions s
        JOIN users u ON u.id = s.user_id
        WHERE s.token_hash = $1 AND s.expires_at > now()`,
        hashToken(token)))
}

// DeleteSession removes a session (logout).
func (r *Repository) DeleteSession(ctx context.Context, token string) error {
    _, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token))
    if err != nil {
        return fmt.Errorf("delete session: %w", err)
    }
    return nil
}

func hashToken(token string) string {
    sum := sha256.Sum256([]byte(token))
    return hex.EncodeToString(sum[:])
}

func randomHex(byteLen int) (string, error) {
    b := make([]byte, byteLen)
    if _, err := rand.Read(b); err != nil {
        return "", fmt.Errorf("generate random token: %w", err)
    }
    return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------------------
// Account deletion
// ---------------------------------------------------------------------------

// DeleteAccount permanently removes a user and every related record in a single
// transaction: likes, comments (both theirs and comments on their posts),
// posts, sessions, and OAuth account records. The schema additionally declares
// ON DELETE CASCADE on every foreign key as a backstop, so no orphaned rows
// can survive even if a future code path forgets an explicit delete.
func (r *Repository) DeleteAccount(ctx context.Context, userID int64) error {
    tx, err := r.pool.Begin(ctx)
    if err != nil {
        return fmt.Errorf("begin: %w", err)
    }
    defer tx.Rollback(ctx) //nolint:errcheck // rollback on commit is a no-op

    statements := []string{
        `DELETE FROM likes WHERE user_id = $1`,
        `DELETE FROM likes WHERE post_id IN (SELECT id FROM posts WHERE author_id = $1)`,
        `DELETE FROM comments WHERE author_id = $1`,
        `DELETE FROM comments WHERE post_id IN (SELECT id FROM posts WHERE author_id = $1)`,
        `DELETE FROM posts WHERE author_id = $1`,
        `DELETE FROM sessions WHERE user_id = $1`,
        `DELETE FROM oauth_accounts WHERE user_id = $1`,
        `DELETE FROM users WHERE id = $1`,
    }
    for _, stmt := range statements {
        if _, err := tx.Exec(ctx, stmt, userID); err != nil {
            return fmt.Errorf("delete account: %w", err)
        }
    }

    var usersLeft int64
    if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, userID).Scan(&usersLeft); err != nil {
        return fmt.Errorf("verify deletion: %w", err)
    }
    if usersLeft != 0 {
        return errors.New("delete account: user row still present after deletion")
    }

    if err := tx.Commit(ctx); err != nil {
        return fmt.Errorf("commit: %w", err)
    }
    return nil
}
