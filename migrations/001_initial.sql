-- Inkwell initial schema.
-- Apply with goose (never run automatically by the web application):
--   goose -dir migrations postgres "$DATABASE_URL" up

-- +goose Up
CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT        NOT NULL,
    display_name  TEXT        NOT NULL,
    avatar_url    TEXT        NOT NULL DEFAULT '',
    github_login  TEXT        NOT NULL DEFAULT '',
    bio_markdown  TEXT        NOT NULL DEFAULT '',
    bio_html      TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT users_username_unique UNIQUE (username),
    CONSTRAINT users_username_format CHECK (username ~ '^[a-z0-9][a-z0-9-]{0,38}$'),
    CONSTRAINT users_display_name_len CHECK (char_length(display_name) BETWEEN 1 AND 120),
    CONSTRAINT users_avatar_len CHECK (char_length(avatar_url) <= 1000),
    CONSTRAINT users_bio_len CHECK (char_length(bio_markdown) <= 2000)
);

CREATE TABLE oauth_accounts (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider            TEXT        NOT NULL,
    provider_account_id TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT oauth_provider_format CHECK (provider IN ('github')),
    CONSTRAINT oauth_provider_account_unique UNIQUE (provider, provider_account_id)
);
CREATE INDEX idx_oauth_accounts_user_id ON oauth_accounts (user_id);

CREATE TABLE sessions (
    token_hash TEXT        NOT NULL,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT sessions_pk PRIMARY KEY (token_hash),
    CONSTRAINT sessions_expires_after_creation CHECK (expires_at > created_at)
);
CREATE INDEX idx_sessions_user_id ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

CREATE TABLE posts (
    id               BIGSERIAL PRIMARY KEY,
    author_id        BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title            TEXT        NOT NULL,
    content_markdown TEXT        NOT NULL,
    content_html     TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT posts_title_len CHECK (char_length(title) BETWEEN 1 AND 120),
    CONSTRAINT posts_content_len CHECK (char_length(content_markdown) BETWEEN 1 AND 20000)
);
CREATE INDEX idx_posts_created_at_id ON posts (created_at DESC, id DESC);
CREATE INDEX idx_posts_author_id ON posts (author_id);

CREATE TABLE comments (
    id               BIGSERIAL PRIMARY KEY,
    post_id          BIGINT      NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    author_id        BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    content_markdown TEXT        NOT NULL,
    content_html     TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT comments_content_len CHECK (char_length(content_markdown) BETWEEN 1 AND 4000)
);
CREATE INDEX idx_comments_post_created ON comments (post_id, created_at, id);
CREATE INDEX idx_comments_author_id ON comments (author_id);

CREATE TABLE likes (
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    post_id    BIGINT      NOT NULL REFERENCES posts (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT likes_pk PRIMARY KEY (user_id, post_id)
);
CREATE INDEX idx_likes_post_id ON likes (post_id);

-- +goose Down
DROP TABLE IF EXISTS likes;
DROP TABLE IF EXISTS comments;
DROP TABLE IF EXISTS posts;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS oauth_accounts;
DROP TABLE IF EXISTS users;
