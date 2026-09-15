// Package models defines the application's data structures.
package models

import (
    "html/template"
    "time"
)

// User is an account on the network.
type User struct {
    ID           int64
    Username     string
    DisplayName  string
    AvatarURL    string
    GitHubLogin  string
    BioMarkdown  string
    BioHTML      template.HTML // sanitized server-side before storage
    CreatedAt    time.Time
    UpdatedAt    time.Time
}

// PostView is a post joined with its author and aggregate counts, ready for
// rendering. ViewerIsAuthor and CSRFToken are filled in by handlers.
type PostView struct {
    ID                int64
    AuthorID          int64
    Title             string
    ContentMarkdown   string
    ContentHTML       template.HTML // sanitized server-side before storage
    CreatedAt         time.Time
    UpdatedAt         time.Time
    AuthorUsername    string
    AuthorDisplayName string
    AuthorAvatarURL   string
    LikeCount         int64
    CommentCount      int64
    LikedByViewer     bool
    ViewerIsAuthor    bool
    CSRFToken         string
}

// CommentView is a comment joined with its author, ready for rendering.
type CommentView struct {
    ID                int64
    PostID            int64
    AuthorID          int64
    ContentHTML       template.HTML // sanitized server-side before storage
    CreatedAt         time.Time
    AuthorUsername    string
    AuthorDisplayName string
    AuthorAvatarURL   string
    ViewerIsAuthor    bool
    CSRFToken         string
}

// CommentRef is the minimal comment data needed for authorization checks.
type CommentRef struct {
    ID       int64
    PostID   int64
    AuthorID int64
}
