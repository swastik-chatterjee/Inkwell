# Third-Party Dependencies

Inkwell uses the following open-source components. We are grateful to the maintainers and contributors of these projects.

## Go Dependencies

### github.com/jackc/pgx/v5
- **Version:** v5.7.1
- **Purpose:** PostgreSQL driver for Go
- **License:** MIT
- **Copyright:** Jack Christensen
- **URL:** https://github.com/jackc/pgx

### github.com/microcosm-cc/bluemonday
- **Version:** v1.0.26
- **Purpose:** HTML sanitization; used to clean user-submitted Markdown-to-HTML output
- **License:** BSD 3-Clause
- **Copyright:** Microcosm CC Ltd.
- **URL:** https://github.com/microcosm-cc/bluemonday

### github.com/yuin/goldmark
- **Version:** v1.7.8
- **Purpose:** Markdown parser and renderer
- **License:** MIT
- **Copyright:** Yuin Kyu
- **URL:** https://github.com/yuin/goldmark

### github.com/pressly/goose/v3
- **Version:** v3.21.1
- **Purpose:** Database migration tool; used by the test suite and for schema management
- **License:** Apache 2.0
- **Copyright:** Pressly, Inc.
- **URL:** https://github.com/pressly/goose

### golang.org/x/oauth2
- **Version:** v0.24.0
- **Purpose:** OAuth2 client implementation for GitHub authentication
- **License:** BSD 3-Clause
- **Copyright:** The Go Authors
- **URL:** https://github.com/golang/oauth2

### Transitive dependencies (selected)

These are dependencies pulled in by the above packages:

- **github.com/aymerick/douceur** (MIT) — CSS parser used by bluemonday
- **github.com/gorilla/css** (BSD 3-Clause) — CSS utilities used by bluemonday
- **github.com/jackc/pgpassfile** (MIT) — PostgreSQL password file support
- **github.com/jackc/pgservicefile** (MIT) — PostgreSQL service file support
- **github.com/jackc/puddle/v2** (MIT) — Connection pool used by pgx
- **golang.org/x/crypto** (BSD 3-Clause) — Cryptographic functions
- **golang.org/x/net** (BSD 3-Clause) — Networking utilities
- **golang.org/x/sync** (BSD 3-Clause) — Synchronization primitives
- **golang.org/x/text** (BSD 3-Clause) — Text processing utilities

All transitive dependencies are managed by Go modules and listed in `go.mod` and `go.sum`.

## Frontend Assets

### No external JavaScript frameworks or libraries

Inkwell's frontend uses vanilla JavaScript (ES6). There are no npm dependencies, no build step, and no bundler.

### CSS

The stylesheet (`web/static/style.css`) is hand-written and uses only CSS features supported in modern browsers (custom properties, grid, flexbox, media queries). No CSS framework or preprocessor is used.

### Fonts

The application uses system fonts only (no external font files):
- **Sans-serif:** system-ui, Segoe UI, Roboto, Helvetica, Arial, Noto Sans
- **Serif:** Iowan Old Style, Palatino, Book Antiqua, Georgia, Times New Roman
- **Monospace:** SF Mono, Cascadia Code, JetBrains Mono, Menlo, Consolas, DejaVu Sans Mono

### Icons

Typographic ornaments and emoji are used for visual accents (e.g., `❦` for section breaks, `♡`/`♥` for likes). No icon library or font is required.

## Deployment & Infrastructure

### Docker

- **Alpine Linux** (base image for minimal runtime)
- No additional system dependencies beyond what Alpine 3.20 provides

### PostgreSQL

- Version 12 or later (tested with PostgreSQL 15+)
- Pure binary protocol; no additional SQL drivers or connectors needed

## License Compliance

- All direct Go dependencies are listed in `go.mod`.
- All dependencies are compatible with the Apache License 2.0 (GPL is not used).
- Source code for all dependencies is available and can be inspected via `go mod graph` or in `$GOPATH/pkg/mod`.

To view the full license of a dependency:

```bash
go mod download
find $GOPATH/pkg/mod -name "LICENSE*" | grep <dependency>
```

## Reporting security issues

If you discover a security vulnerability in a dependency:

1. Check if the issue has been disclosed to the dependency's maintainers.
2. Run `go list -u -m all` to check for available updates.
3. Update vulnerable dependencies with `go get -u <module>@<version>`.
4. Report the issue to Inkwell's maintainers if an update is not yet available.

## Contributing

When adding new dependencies, please:

1. Prefer small, focused libraries over large frameworks.
2. Verify the license is compatible with Apache 2.0.
3. Check for security advisories (`go list -u -m all` and GitHub advisories).
4. Run `go mod tidy` and commit changes to `go.mod` and `go.sum`.
5. Document the dependency in this file.
