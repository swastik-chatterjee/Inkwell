# Inkwell

Inkwell is a markdown-first social network for thoughtful discussion. Users write posts in Markdown, comment on each other's posts, and engage through likes. All content is rendered server-side for reliability and simplicity.

## Features

- **Markdown posts and comments** — Write with full Markdown support, including code blocks, tables, and block quotes.
- **Server-rendered, no framework** — Traditional MPA architecture. Every page is HTML rendered on the server. JavaScript is strictly progressive enhancement.
- **GitHub sign-in** — OAuth2-based authentication with GitHub. No passwords stored.
- **Content moderation** — Automated safety checks using Mistral AI to flag potential harassment and abuse before publishing.
- **Like and comment** — Engage with posts through likes and threaded comments.
- **Public profiles** — Each user has a public profile with a bio and post history.
- **User-controlled account deletion** — Permanent, immediate account removal with cascading deletion of posts, comments, and likes.

## Stack

- **Backend:** Go 1.23 with PostgreSQL
- **Frontend:** HTML + CSS + vanilla JavaScript (progressive enhancement only)
- **Build:** Multi-stage Docker build
- **Deployment:** Container-based (tested on SnapDeploy)

## Getting started

### Prerequisites

- Docker and Docker Compose (recommended)
- OR: Go 1.23+, PostgreSQL 12+, and `goose` for migrations

### Environment configuration

Copy `.env.example` to `.env` and fill in real values:

```bash
cp .env.example .env
```

Required variables for production:
- `DATABASE_URL` — PostgreSQL connection string
- `GITHUB_CLIENT_ID` — GitHub OAuth app credentials
- `GITHUB_CLIENT_SECRET` — GitHub OAuth app credentials
- `SESSION_SECRET` — Secret for signing session cookies (generate with `openssl rand -hex 32`)
- `MISTRAL_API_KEY` — API key for content moderation
- `ENVIRONMENT` — Set to `production` for secure cookies and hardened validation

### Running locally

Using Docker Compose (easiest):

```bash
docker compose up
```

The application will be available at `http://localhost:8080`.

Using Go directly:

```bash
go run ./cmd/web
```

The server listens on port 8080 by default (configurable via `PORT` env var).

### Database migrations

Migrations run automatically on application startup using `goose/v3`. To apply migrations manually:

```bash
goose -dir migrations postgres "$DATABASE_URL" up
```

### Production deployment

Docker image:

```dockerfile
# Already configured in ./Dockerfile
# Multi-stage: builds Go binary on Alpine, runs on minimal Alpine runtime
docker build -t inkwell:latest .
```

The production image:
- Runs as unprivileged user (uid 10001)
- Exposes port 8080
- Requires all environment variables (no defaults)
- Enables secure cookies and CSP headers
- Validates all inputs strictly

## Architecture

### Structure

- **cmd/web/main.go** — Application entry point. Wires dependencies and starts HTTP server.
- **internal/handlers/** — HTTP handlers and route definitions.
- **internal/auth/** — GitHub OAuth and session management.
- **internal/database/** — PostgreSQL connection pool management.
- **internal/repository/** — Data access layer for posts, users, comments, likes.
- **internal/models/** — Domain types.
- **internal/middleware/** — Request logging, panic recovery, CSRF protection, security headers.
- **internal/markdown/** — Markdown parsing and sanitization.
- **internal/moderation/** — Content moderation via Mistral AI.
- **internal/config/** — Environment-based configuration.
- **migrations/** — Database schema (goose format).
- **web/templates/** — HTML templates (server-rendered).
- **web/static/** — CSS and JavaScript.

### Request flow

1. HTTP request arrives at the HTTP server.
2. **Middleware stack** applies security headers, recovers panics, logs requests, validates CSRF, and resolves user from session.
3. **Handlers** parse and validate input, enforce authorization, call business logic (auth, moderation, markdown rendering), and delegate to repository for data mutations.
4. **Repository** executes database operations in transactions where applicable.
5. **Templates** render the response as HTML and send it to the client.
6. **JavaScript** (opt-in) progressively enhances forms, previews, and interactions without requiring server changes.

### Session and authentication

- Users log in via GitHub OAuth2.
- A session token is stored in a signed, HttpOnly, Secure (production) cookie.
- The session cookie is validated on every request; if valid, the user is loaded from the database.
- CSRF tokens are issued on safe (GET, HEAD, OPTIONS) requests and validated on unsafe (POST, PUT, DELETE) requests using the double-submit pattern.

### Content safety

- All user-submitted content (posts, comments, bios) is validated for length, trimmed, and checked for harassment/abuse using Mistral AI.
- If the moderation API is unavailable, the submission fails closed (no content published) and the user sees a clear message.
- Markdown is rendered and sanitized server-side with Goldmark + bluemonday; no user HTML is ever output.
- A client-side Markdown preview is provided for UX, but the server is the authority on what HTML is shown.

### Database

- PostgreSQL 12+.
- Connection pool with configurable lifetime and health-check settings.
- Statement timeout of 15 seconds (server-side guard).
- All queries use parameterized queries (pgx/v5) to prevent SQL injection.
- Migrations are version-controlled in `migrations/` and applied automatically on startup.

## Development

### Build and test

```bash
# Format and lint
go fmt ./...
go vet ./...

# Run tests
go test ./...

# Build binary
go build -o ./markdown-social ./cmd/web

# Run
./markdown-social
```

### Environment for development

Development mode:
- Cookies are not marked Secure (allow HTTP localhost).
- CSRF tokens are less strict (allows development flexibility).
- Missing GitHub OAuth / Mistral credentials trigger warnings but do not fail startup.
- An ephemeral session secret is generated if not provided (sessions do not survive restarts).

Production mode:
- All environment variables are required.
- Cookies are marked Secure and HttpOnly.
- CSP headers forbid inline scripts and styles.
- Strict input validation on all forms.

### Code organization guidelines

- Handlers parse input and enforce authorization; business rules belong in the repository or service packages.
- Errors are logged server-side; user-facing messages are always generic ("Something went wrong").
- Templates are compiled once at startup; render failures are logged and return 500.
- Middleware does not skip handlers; all handlers run through the full middleware stack.

## Deployment

### Container-based deployment (SnapDeploy, Docker Swarm, Kubernetes)

```bash
docker build -t inkwell:latest .
docker run -d \
  -e DATABASE_URL="postgres://..." \
  -e GITHUB_CLIENT_ID="..." \
  -e GITHUB_CLIENT_SECRET="..." \
  -e SESSION_SECRET="..." \
  -e MISTRAL_API_KEY="..." \
  -e ENVIRONMENT="production" \
  -e APP_URL="https://inkwell.example.com" \
  -p 8080:8080 \
  inkwell:latest
```

### Health checks

The application exposes no dedicated health check endpoint; HTTP requests are processed normally. For container health monitoring, check that the server responds to `GET /` with a 200 status code within a reasonable timeout (5 seconds).

### Cold-start behavior

- On startup, the application loads configuration, connects to PostgreSQL, runs migrations, and starts the HTTP server.
- All database connections are established and tested before the server begins accepting requests.
- Request handlers are initialized and templates are compiled once.
- Moderation (Mistral) is called on-demand for each post, comment, or bio update; it does not need to be available at startup.

## License

Inkwell is licensed under the Apache License 2.0. See LICENSE.txt for the full text.

## Contributing

See CONTRIBUTING.md for guidelines on reporting bugs, suggesting features, and submitting patches.

## Code of Conduct

We are committed to creating a welcoming, inclusive community. See CODE_OF_CONDUCT.md for our standards and reporting procedures.
