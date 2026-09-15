package tests

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"

	"markdown-social/internal/models"
	"markdown-social/internal/repository"
)

// newTestDB skips when TEST_DATABASE_URL is not set, applies the real Goose
// migrations, and returns a repository over a clean database.
func newTestDB(t *testing.T) (*repository.Repository, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping database-backed tests")
	}

	sqlDB, err := sql.Open(stdlib.GetDefaultDriver(), databaseURL)
	if err != nil {
		t.Fatalf("open sql connection: %v", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
	if err := goose.Up(sqlDB, "../migrations"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	truncate := func() {
		_, _ = pool.Exec(context.Background(),
			"TRUNCATE users, oauth_accounts, sessions, posts, comments, likes RESTART IDENTITY CASCADE")
	}
	truncate()
	t.Cleanup(truncate) // runs before pool.Close (cleanups are LIFO)

	return repository.New(pool), pool
}

func mustCreateUser(t *testing.T, repo *repository.Repository, login string, githubID int64) *models.User {
	t.Helper()
	user, err := repo.UpsertGitHubUser(context.Background(), login, githubID, "Display "+login, "https://avatars.example/"+login+".png")
	if err != nil {
		t.Fatalf("create user %s: %v", login, err)
	}
	return user
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func TestLikeUniqueness(t *testing.T) {
	repo, pool := newTestDB(t)
	ctx := context.Background()

	alice := mustCreateUser(t, repo, "alice", 101)
	bob := mustCreateUser(t, repo, "bob", 102)
	postID, err := repo.CreatePost(ctx, alice.ID, "Alice's post", "Hello", "<p>Hello</p>")
	if err != nil {
		t.Fatalf("create post: %v", err)
	}

	// Liking twice must neither error nor create a second row.
	if err := repo.LikePost(ctx, bob.ID, postID); err != nil {
		t.Fatalf("like: %v", err)
	}
	if err := repo.LikePost(ctx, bob.ID, postID); err != nil {
		t.Fatalf("second like: %v", err)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM likes WHERE post_id = $1", postID); got != 1 {
		t.Fatalf("likes = %d, want 1 (database must enforce uniqueness)", got)
	}

	// Concurrent likes from the same viewer must still converge to one row.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = repo.LikePost(ctx, bob.ID, postID)
		}()
	}
	wg.Wait()
	if got := countRows(t, pool, "SELECT count(*) FROM likes WHERE post_id = $1", postID); got != 1 {
		t.Fatalf("likes after concurrent calls = %d, want 1", got)
	}

	post, err := repo.GetPost(ctx, postID, bob.ID)
	if err != nil {
		t.Fatalf("get post: %v", err)
	}
	if post.LikeCount != 1 {
		t.Fatalf("LikeCount = %d, want 1", post.LikeCount)
	}
	if !post.LikedByViewer {
		t.Fatal("LikedByViewer = false, want true")
	}

	// Unliking is idempotent too.
	if err := repo.UnlikePost(ctx, bob.ID, postID); err != nil {
		t.Fatalf("unlike: %v", err)
	}
	if err := repo.UnlikePost(ctx, bob.ID, postID); err != nil {
		t.Fatalf("second unlike: %v", err)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM likes WHERE post_id = $1", postID); got != 0 {
		t.Fatalf("likes after unlike = %d, want 0", got)
	}
}

func TestPostAuthorization(t *testing.T) {
	repo, _ := newTestDB(t)
	ctx := context.Background()

	alice := mustCreateUser(t, repo, "alice", 101)
	bob := mustCreateUser(t, repo, "bob", 102)
	postID, err := repo.CreatePost(ctx, alice.ID, "Original", "Original body", "<p>Original body</p>")
	if err != nil {
		t.Fatalf("create post: %v", err)
	}

	updated, err := repo.UpdatePost(ctx, postID, bob.ID, "Hacked", "Hacked", "<p>Hacked</p>")
	if err != nil {
		t.Fatalf("update post as bob: %v", err)
	}
	if updated {
		t.Fatal("bob updated alice's post; want authorization failure")
	}

	deleted, err := repo.DeletePost(ctx, postID, bob.ID)
	if err != nil {
		t.Fatalf("delete post as bob: %v", err)
	}
	if deleted {
		t.Fatal("bob deleted alice's post; want authorization failure")
	}

	post, err := repo.GetPost(ctx, postID, 0)
	if err != nil {
		t.Fatalf("get post: %v", err)
	}
	if post.Title != "Original" || post.ContentMarkdown != "Original body" {
		t.Fatalf("post was modified by an unauthorized user: %q", post.Title)
	}

	deleted, err = repo.DeletePost(ctx, postID, alice.ID)
	if err != nil {
		t.Fatalf("delete post as alice: %v", err)
	}
	if !deleted {
		t.Fatal("alice could not delete her own post")
	}
	if _, err := repo.GetPost(ctx, postID, 0); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("get deleted post: err = %v, want ErrNotFound", err)
	}
}

func TestCommentAuthorization(t *testing.T) {
	repo, _ := newTestDB(t)
	ctx := context.Background()

	alice := mustCreateUser(t, repo, "alice", 101)
	bob := mustCreateUser(t, repo, "bob", 102)
	postID, err := repo.CreatePost(ctx, alice.ID, "Post", "Body", "<p>Body</p>")
	if err != nil {
		t.Fatalf("create post: %v", err)
	}
	commentID, err := repo.CreateComment(ctx, postID, bob.ID, "Bob's comment", "<p>Bob's comment</p>")
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}

	// Alice cannot delete Bob's comment.
	deleted, err := repo.DeleteComment(ctx, commentID, alice.ID)
	if err != nil {
		t.Fatalf("delete comment as alice: %v", err)
	}
	if deleted {
		t.Fatal("alice deleted bob's comment; want authorization failure")
	}
	if _, err := repo.GetComment(ctx, commentID); err != nil {
		t.Fatalf("comment vanished after unauthorized delete: %v", err)
	}

	// Bob can delete his own.
	deleted, err = repo.DeleteComment(ctx, commentID, bob.ID)
	if err != nil {
		t.Fatalf("delete comment as bob: %v", err)
	}
	if !deleted {
		t.Fatal("bob could not delete his own comment")
	}
	if _, err := repo.GetComment(ctx, commentID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("get deleted comment: err = %v, want ErrNotFound", err)
	}
}

func TestAccountDeletionIsTransactionalAndComplete(t *testing.T) {
	repo, pool := newTestDB(t)
	ctx := context.Background()

	alice := mustCreateUser(t, repo, "alice", 101)
	bob := mustCreateUser(t, repo, "bob", 102)

	// Alice: one post, one comment on Bob's post, one like on Bob's post,
	// one like received on her own post, and an active session.
	postA, err := repo.CreatePost(ctx, alice.ID, "Alice's post", "A", "<p>A</p>")
	if err != nil {
		t.Fatalf("create postA: %v", err)
	}
	postB, err := repo.CreatePost(ctx, bob.ID, "Bob's post", "B", "<p>B</p>")
	if err != nil {
		t.Fatalf("create postB: %v", err)
	}
	if _, err := repo.CreateComment(ctx, postB, alice.ID, "Alice on B", "<p>Alice on B</p>"); err != nil {
		t.Fatalf("create alice comment: %v", err)
	}
	if _, err := repo.CreateComment(ctx, postA, bob.ID, "Bob on A", "<p>Bob on A</p>"); err != nil {
		t.Fatalf("create bob comment: %v", err)
	}
	if err := repo.LikePost(ctx, alice.ID, postB); err != nil {
		t.Fatalf("alice likes postB: %v", err)
	}
	if err := repo.LikePost(ctx, bob.ID, postA); err != nil {
		t.Fatalf("bob likes postA: %v", err)
	}
	token, _, err := repo.CreateSession(ctx, alice.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := repo.DeleteAccount(ctx, alice.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	// Alice and everything she owned is gone.
	if _, err := repo.GetUserByID(ctx, alice.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("user still exists after deletion: err = %v", err)
	}
	if _, err := repo.UserForSession(ctx, token); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("session still resolves after deletion: err = %v", err)
	}
	if _, err := repo.GetPost(ctx, postA, 0); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("postA still exists after deletion: err = %v", err)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM posts WHERE author_id = $1", alice.ID); got != 0 {
		t.Fatalf("posts by alice = %d, want 0", got)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM comments WHERE author_id = $1", alice.ID); got != 0 {
		t.Fatalf("comments by alice = %d, want 0", got)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM likes WHERE user_id = $1", alice.ID); got != 0 {
		t.Fatalf("likes by alice = %d, want 0", got)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM sessions WHERE user_id = $1", alice.ID); got != 0 {
		t.Fatalf("sessions for alice = %d, want 0", got)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM oauth_accounts WHERE user_id = $1", alice.ID); got != 0 {
		t.Fatalf("oauth accounts for alice = %d, want 0", got)
	}
	// Comments on Alice's post (Bob's) were removed with the post.
	if got := countRows(t, pool, "SELECT count(*) FROM comments WHERE post_id = $1", postA); got != 0 {
		t.Fatalf("comments on alice's deleted post = %d, want 0", got)
	}

	// Bob's surviving data is intact, but the like and comment from Alice
	// are gone, and no orphaned rows remain anywhere.
	if _, err := repo.GetUserByID(ctx, bob.ID); err != nil {
		t.Fatalf("bob was deleted: %v", err)
	}
	post, err := repo.GetPost(ctx, postB, bob.ID)
	if err != nil {
		t.Fatalf("get postB: %v", err)
	}
	if post.LikeCount != 0 {
		t.Fatalf("postB like count = %d, want 0 (alice's like removed)", post.LikeCount)
	}
	if post.CommentCount != 0 {
		t.Fatalf("postB comment count = %d, want 0 (alice's comment removed)", post.CommentCount)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM comments c LEFT JOIN posts p ON p.id = c.post_id WHERE p.id IS NULL"); got != 0 {
		t.Fatalf("orphaned comments = %d, want 0", got)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM likes l LEFT JOIN posts p ON p.id = l.post_id WHERE p.id IS NULL"); got != 0 {
		t.Fatalf("orphaned likes = %d, want 0", got)
	}
	if got := countRows(t, pool, "SELECT count(*) FROM sessions s LEFT JOIN users u ON u.id = s.user_id WHERE u.id IS NULL"); got != 0 {
		t.Fatalf("orphaned sessions = %d, want 0", got)
	}
}

func TestSessionLifecycle(t *testing.T) {
	repo, pool := newTestDB(t)
	ctx := context.Background()

	alice := mustCreateUser(t, repo, "alice", 101)
	token, expires, err := repo.CreateSession(ctx, alice.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if token == "" {
		t.Fatal("session token is empty")
	}
	if !expires.After(time.Now()) {
		t.Fatalf("session expires in the past: %v", expires)
	}

	user, err := repo.UserForSession(ctx, token)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	if user.ID != alice.ID {
		t.Fatalf("session resolved to user %d, want %d", user.ID, alice.ID)
	}

	// Expired sessions must not resolve.
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 hour' WHERE user_id = $1", alice.ID); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	if _, err := repo.UserForSession(ctx, token); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("expired session resolved: err = %v, want ErrNotFound", err)
	}

	// Logout invalidates the session row.
	token2, _, err := repo.CreateSession(ctx, alice.ID)
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if err := repo.DeleteSession(ctx, token2); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := repo.UserForSession(ctx, token2); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("deleted session resolved: err = %v, want ErrNotFound", err)
	}
}

func TestUpsertGitHubUser(t *testing.T) {
	repo, pool := newTestDB(t)
	ctx := context.Background()

	// New account.
	first, err := repo.UpsertGitHubUser(ctx, "octocat", 1, "Octo Cat", "https://avatars.example/o.png")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.Username != "octocat" {
		t.Fatalf("username = %q, want octocat", first.Username)
	}

	// Same GitHub account: profile refreshed, same local user.
	second, err := repo.UpsertGitHubUser(ctx, "octocat", 1, "Octo Renamed", "https://avatars.example/o2.png")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("same GitHub account mapped to user %d, want %d", second.ID, first.ID)
	}
	if second.DisplayName != "Octo Renamed" || second.AvatarURL != "https://avatars.example/o2.png" {
		t.Fatalf("profile not refreshed: %q %q", second.DisplayName, second.AvatarURL)
	}

	// A different GitHub account with the same login gets a suffixed username.
	third, err := repo.UpsertGitHubUser(ctx, "octocat", 2, "Other Octo", "")
	if err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if third.ID == first.ID {
		t.Fatal("different GitHub account mapped to the same user")
	}
	if third.Username != "octocat-2" {
		t.Fatalf("username = %q, want octocat-2", third.Username)
	}

	// Logins that are not valid usernames are sanitized.
	fourth, err := repo.UpsertGitHubUser(ctx, "Weird_Login!", 3, "Weird", "")
	if err != nil {
		t.Fatalf("fourth upsert: %v", err)
	}
	if fourth.Username != "weird-login" {
		t.Fatalf("username = %q, want weird-login", fourth.Username)
	}

	// The (provider, provider_account_id) pair is enforced by the database.
	_, err = pool.Exec(ctx,
		"INSERT INTO oauth_accounts (user_id, provider, provider_account_id) VALUES ($1, 'github', '1')",
		third.ID)
	if err == nil {
		t.Fatal("duplicate oauth account inserted; want unique constraint violation")
	}
}

func TestUpdateBio(t *testing.T) {
	repo, _ := newTestDB(t)
	ctx := context.Background()

	alice := mustCreateUser(t, repo, "alice", 101)
	if err := repo.UpdateBio(ctx, alice.ID, "Hello *bio*", "<p>Hello <em>bio</em></p>"); err != nil {
		t.Fatalf("update bio: %v", err)
	}
	got, err := repo.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.BioMarkdown != "Hello *bio*" {
		t.Fatalf("BioMarkdown = %q", got.BioMarkdown)
	}
	if string(got.BioHTML) != "<p>Hello <em>bio</em></p>" {
		t.Fatalf("BioHTML = %q", string(got.BioHTML))
	}

	// Unknown user: not found, not silently ignored.
	if err := repo.UpdateBio(ctx, 999999, "x", "y"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("update bio for missing user: err = %v, want ErrNotFound", err)
	}
}
