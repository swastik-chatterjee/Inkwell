# Contributing to Inkwell

Thank you for your interest in contributing to Inkwell! This document provides guidelines for reporting issues, suggesting features, and submitting code changes.

## Reporting bugs

If you discover a bug:

1. **Check existing issues** — Search the issue tracker to see if the bug has already been reported.
2. **Provide details** — Include:
   - Steps to reproduce
   - Expected behavior
   - Actual behavior
   - Environment (Go version, OS, PostgreSQL version)
   - Relevant logs or error messages
3. **Be specific** — Vague reports ("it doesn't work") are hard to act on; specific examples help.

## Suggesting features

Feature suggestions are welcome:

1. **Check existing issues** — Search to avoid duplicates.
2. **Describe the use case** — Explain the problem you're trying to solve and why it matters.
3. **Propose a solution** — Outline how the feature might work (UI, API, configuration, etc.).
4. **Consider alternatives** — Discuss why alternative approaches might not be suitable.

We prioritize features that:
- Align with Inkwell's design philosophy (simple, server-rendered, content-first)
- Solve real problems for users
- Do not add excessive complexity or dependencies
- Can be implemented reliably within the existing architecture

## Submitting code

### Before you start

- Read the architecture section in README.md to understand the codebase structure.
- Familiarize yourself with the existing code style and patterns.
- Check the issue tracker for related discussions or prior work.
- For large changes, open an issue first to discuss the approach.

### Development workflow

1. **Fork the repository** and clone your fork locally.
2. **Create a feature branch** from `master`:
   ```bash
   git checkout -b feature/your-feature-name
   ```
3. **Make your changes**:
   - Write clean, readable code that follows Go conventions.
   - Add or update tests as appropriate.
   - Keep commits atomic and well-messages.
4. **Test locally**:
   ```bash
   go fmt ./...
   go vet ./...
   go test ./...
   go build -o ./markdown-social ./cmd/web
   ```
5. **Run the application** and verify your changes work end-to-end.
6. **Commit and push** your changes to your fork.
7. **Open a pull request** against `master` with a clear title and description.

### Pull request guidelines

- **Title** — Summarize the change in one line (e.g., "Fix publish race condition during container cold-start").
- **Description** — Explain:
  - What the change does
  - Why it is needed
  - How it was tested
  - Any trade-offs or limitations
- **Scope** — Keep PRs focused on a single concern. Large changes should be broken into smaller PRs.
- **No merge conflicts** — Rebase on `master` if needed.
- **Documentation** — Update README.md, comments, or other docs if behavior changes.

### Code style

- Follow Go conventions (go fmt, idiomatic error handling, etc.).
- Use meaningful variable and function names.
- Add comments for non-obvious logic.
- Keep functions focused and reasonably sized.
- Avoid unnecessary dependencies.

### Testing

- Write tests for new features and bug fixes.
- Existing tests must pass (`go test ./...`).
- Test both success and failure paths.
- Use table-driven tests for multiple cases.
- Aim for clear, readable test names that describe what is being tested.

### Commit messages

- Write clear, descriptive commit messages in the imperative mood ("Add feature", not "Added feature").
- Reference related issues when applicable ("Fixes #123").
- Keep the first line under 72 characters; add details in the body if needed.

### Licensing

By contributing code, you agree that your contributions will be licensed under the Apache License 2.0 (see LICENSE.txt). You confirm that you own or have the right to contribute the code, and that your contribution does not violate any third-party intellectual property rights.

## Code review process

1. A maintainer will review your PR within a reasonable timeframe.
2. Feedback will be provided as comments on the PR.
3. Please respond to feedback with clarifications or updated code.
4. Once approved, your PR will be merged into `master`.

## Recognition

Contributors will be recognized in the project. If you prefer to remain anonymous, please let us know.

## Questions?

If you have questions, open an issue and label it as a question. We're happy to help!

Thank you for contributing to Inkwell. 🙏
