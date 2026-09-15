// Package markdown renders user-supplied Markdown into sanitized HTML.
//
// The pipeline is deliberately defense-in-depth:
//  1. Goldmark parses Markdown with the GFM extension set. Raw HTML in the
//     source is escaped by default (html.WithUnsafe is NOT enabled), so
//     <script>, <iframe> and friends never survive parsing as markup.
//  2. The rendered HTML is passed through bluemonday's UGCPolicy, which strips
//     event-handler attributes, javascript: URLs, unsafe embedded content and
//     any other dangerous markup, and adds rel="nofollow" to links.
//
// Only the sanitized result is ever stored or rendered into templates.
package markdown

import (
    "bytes"
    "fmt"
    "html/template"

    "github.com/microcosm-cc/bluemonday"
    "github.com/yuin/goldmark"
    "github.com/yuin/goldmark/extension"
)

// Renderer converts Markdown source into template-safe HTML.
type Renderer struct {
    parser goldmark.Markdown
    policy *bluemonday.Policy
}

// New builds a Renderer. It returns an error if the sanitizer cannot be
// initialized.
func New() (*Renderer, error) {
    policy := bluemonday.UGCPolicy()
    if policy == nil {
        return nil, fmt.Errorf("markdown: sanitizer policy could not be initialized")
    }
    parser := goldmark.New(
        goldmark.WithExtensions(
            extension.GFM, // tables, strikethrough, linkified URLs
        ),
    )
    return &Renderer{parser: parser, policy: policy}, nil
}

// Render converts Markdown to sanitized HTML. The returned template.HTML is
// safe to interpolate into templates because it has already been sanitized.
func (r *Renderer) Render(source string) (template.HTML, error) {
    var raw bytes.Buffer
    if err := r.parser.Convert([]byte(source), &raw); err != nil {
        return "", fmt.Errorf("markdown: parse: %w", err)
    }
    sanitized := r.policy.SanitizeBytes(raw.Bytes())
    return template.HTML(sanitized), nil
}
