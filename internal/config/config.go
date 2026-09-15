// Package config loads and validates application configuration from
// environment variables. Secrets are never hard-coded and never logged.
package config

import (
    "crypto/rand"
    "encoding/hex"
    "errors"
    "fmt"
    "os"
    "strconv"
    "strings"
)

const (
    EnvDevelopment = "development"
    EnvProduction  = "production"
)

// Config carries every runtime setting for the application.
type Config struct {
    DatabaseURL        string
    GitHubClientID     string
    GitHubClientSecret string
    GitHubRedirectURL  string
    SessionSecret      string
    AppURL             string
    Port               string
    Environment        string
    MistralAPIKey      string
    MistralModel       string
}

// Load reads the environment, applies safe development defaults, and enforces
// required variables. In production every secret must be provided explicitly.
func Load() (*Config, error) {
    cfg := &Config{
        DatabaseURL:        strings.TrimSpace(os.Getenv("DATABASE_URL")),
        GitHubClientID:     strings.TrimSpace(os.Getenv("GITHUB_CLIENT_ID")),
        GitHubClientSecret: strings.TrimSpace(os.Getenv("GITHUB_CLIENT_SECRET")),
        GitHubRedirectURL:  strings.TrimSpace(os.Getenv("GITHUB_REDIRECT_URL")),
        SessionSecret:      strings.TrimSpace(os.Getenv("SESSION_SECRET")),
        AppURL:             envDefault("APP_URL", "http://localhost:8080"),
        Port:               envDefault("PORT", "8080"),
        Environment:        envDefault("ENVIRONMENT", EnvDevelopment),
        MistralAPIKey:      strings.TrimSpace(os.Getenv("MISTRAL_API_KEY")),
        MistralModel:       envDefault("MISTRAL_MODEL", "mistral-small-latest"),
    }

    cfg.Environment = strings.ToLower(strings.TrimSpace(cfg.Environment))
    switch cfg.Environment {
    case "", "dev":
        cfg.Environment = EnvDevelopment
    case "prod":
        cfg.Environment = EnvProduction
    case EnvDevelopment, EnvProduction:
    default:
        return nil, fmt.Errorf("invalid ENVIRONMENT %q (use development or production)", cfg.Environment)
    }

    if _, err := strconv.Atoi(cfg.Port); err != nil {
        return nil, fmt.Errorf("invalid PORT %q", cfg.Port)
    }
    cfg.AppURL = strings.TrimRight(cfg.AppURL, "/")

    if cfg.DatabaseURL == "" {
        return nil, errors.New("DATABASE_URL is required")
    }

    if cfg.GitHubRedirectURL == "" {
        cfg.GitHubRedirectURL = cfg.AppURL + "/auth/github/callback"
    }

    if cfg.IsProduction() {
        var missing []string
        if cfg.GitHubClientID == "" {
            missing = append(missing, "GITHUB_CLIENT_ID")
        }
        if cfg.GitHubClientSecret == "" {
            missing = append(missing, "GITHUB_CLIENT_SECRET")
        }
        if cfg.GitHubRedirectURL == "" {
            missing = append(missing, "GITHUB_REDIRECT_URL")
        }
        if cfg.SessionSecret == "" {
            missing = append(missing, "SESSION_SECRET")
        }
        if cfg.MistralAPIKey == "" {
            missing = append(missing, "MISTRAL_API_KEY")
        }
        if len(missing) > 0 {
            return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
        }
    } else {
        if cfg.SessionSecret == "" {
            secret, err := randomHex(32)
            if err != nil {
                return nil, fmt.Errorf("generate ephemeral session secret: %w", err)
            }
            cfg.SessionSecret = secret
            warn("SESSION_SECRET is not set; using a random ephemeral secret. Sessions will not survive restarts.")
        }
        if cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "" {
            warn("GITHUB_CLIENT_ID / GITHUB_CLIENT_SECRET are not set; GitHub login will not work.")
        }
        if cfg.MistralAPIKey == "" {
            warn("MISTRAL_API_KEY is not set; moderation will fail closed and no new content can be published.")
        }
    }

    return cfg, nil
}

// IsProduction reports whether the server runs with production hardening.
func (c *Config) IsProduction() bool {
    return c.Environment == EnvProduction
}

func envDefault(key, fallback string) string {
    if v := strings.TrimSpace(os.Getenv(key)); v != "" {
        return v
    }
    return fallback
}

func warn(message string) {
    fmt.Fprintf(os.Stderr, "config: warning: %s\n", message)
}

func randomHex(byteLen int) (string, error) {
    b := make([]byte, byteLen)
    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return hex.EncodeToString(b), nil
}
