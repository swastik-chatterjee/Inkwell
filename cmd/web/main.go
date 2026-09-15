// Command web runs the Inkwell markdown social network server.
//
// It is a traditional server-rendered Multi-Page Application (MPA): every page
// is rendered with html/template on the server, and the browser receives plain
// HTML documents. JavaScript is strictly progressive enhancement.
package main

import (
    "context"
    "errors"
    "fmt"
    "log/slog"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "golang.org/x/oauth2"
    "golang.org/x/oauth2/github"

    "markdown-social/internal/auth"
    "markdown-social/internal/config"
    "markdown-social/internal/database"
    "markdown-social/internal/handlers"
    "markdown-social/internal/markdown"
    "markdown-social/internal/middleware"
    "markdown-social/internal/moderation"
    "markdown-social/internal/repository"
)

func main() {
    if err := run(); err != nil {
        slog.Error("fatal", "error", err)
        os.Exit(1)
    }
}

func run() error {
    cfg, err := config.Load()
    if err != nil {
        return err
    }

    logger := newLogger(cfg)
    slog.SetDefault(logger)
    logger.Info("starting inkwell",
        "environment", cfg.Environment,
        "port", cfg.Port,
        "app_url", cfg.AppURL,
    )

    ctx := context.Background()

    pool, err := database.Connect(ctx, cfg.DatabaseURL)
    if err != nil {
        return fmt.Errorf("connect to database: %w", err)
    }
    defer pool.Close()

    repo := repository.New(pool)
    signer := auth.NewSigner(cfg.SessionSecret)

    githubOAuth := &oauth2.Config{
        ClientID:     cfg.GitHubClientID,
        ClientSecret: cfg.GitHubClientSecret,
        Endpoint:     github.Endpoint,
        RedirectURL:  cfg.GitHubRedirectURL,
        Scopes:       []string{"read:user"},
    }
    authSvc := auth.New(githubOAuth, repo, signer, cfg.IsProduction(), logger)

    renderer, err := markdown.New()
    if err != nil {
        return fmt.Errorf("initialize markdown renderer: %w", err)
    }

    moderator := moderation.New(nil, cfg.MistralAPIKey, cfg.MistralModel, moderation.DefaultEndpoint, logger)

    h, err := handlers.New(handlers.Deps{
        Repo:        repo,
        Auth:        authSvc,
        Moderation:  moderator,
        Markdown:    renderer,
        Signer:      signer,
        Config:      cfg,
        Log:         logger,
        TemplateDir: "web/templates",
        StaticDir:   "web/static",
    })
    if err != nil {
        return fmt.Errorf("initialize handlers: %w", err)
    }

    mw := middleware.New(logger, authSvc, cfg.IsProduction())

    server := &http.Server{
        Addr:              ":" + cfg.Port,
        Handler:           mw.Wrap(h.Routes()),
        ReadHeaderTimeout: 10 * time.Second,
        ReadTimeout:       30 * time.Second,
        WriteTimeout:      60 * time.Second,
        IdleTimeout:       120 * time.Second,
        MaxHeaderBytes:    1 << 20,
    }

    serverErr := make(chan error, 1)
    go func() {
        logger.Info("listening", "addr", server.Addr)
        if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            serverErr <- err
        }
    }()

    stopCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
    defer stop()

    select {
    case err := <-serverErr:
        return fmt.Errorf("http server: %w", err)
    case <-stopCtx.Done():
        logger.Info("shutdown signal received")
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()
        if err := server.Shutdown(shutdownCtx); err != nil {
            return fmt.Errorf("graceful shutdown: %w", err)
        }
        pool.Close()
        logger.Info("shutdown complete")
    }
    return nil
}

func newLogger(cfg *config.Config) *slog.Logger {
    if cfg.IsProduction() {
        return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
    }
    return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
