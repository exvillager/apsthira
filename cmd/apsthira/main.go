package main

import (
	"context"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/exvillager/nanoserve"
	"github.com/joho/godotenv"
	"golang.org/x/time/rate"

	"apsthira/internal/config"
	"apsthira/internal/db"
	"apsthira/internal/handler"
	"apsthira/internal/logger"
	"apsthira/internal/middleware"
	"apsthira/internal/storage"
)

//go:embed templates/*
var templatesFS embed.FS

func main() {
	logger.Init()
	_ = godotenv.Load()

	// Check if sync command is requested
	if len(os.Args) > 1 && (os.Args[1] == "--sync" || os.Args[1] == "--sync-push" || os.Args[1] == "--sync-pull") {
		sqlitePath := config.GetEnv("DB_PATH", "resumes.db")
		postgresURL := os.Getenv("DATABASE_URL")
		if postgresURL == "" {
			postgresURL = os.Getenv("SUPABASE_DB_URL")
		}

		if postgresURL == "" {
			slog.Error("sync failed: target database URL not provided. Please set DATABASE_URL or SUPABASE_DB_URL in your env variables.")
			os.Exit(1)
		}

		isSQLitePostgres := strings.HasPrefix(sqlitePath, "postgres://") || strings.HasPrefix(sqlitePath, "postgresql://")
		if isSQLitePostgres {
			slog.Error("sync failed: DB_PATH must point to a local SQLite database file, not a postgres connection string.")
			os.Exit(1)
		}

		var err error
		if os.Args[1] == "--sync-pull" {
			err = db.SyncPostgresToSQLite(sqlitePath, postgresURL)
		} else {
			err = db.SyncSQLiteToPostgres(sqlitePath, postgresURL)
		}

		if err != nil {
			slog.Error("sync execution failed", "error", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	port := config.GetEnv("PORT", "8080")
	dbConnStr := os.Getenv("DATABASE_URL")
	if dbConnStr == "" {
		dbConnStr = config.GetEnv("DB_PATH", "resumes.db")
	}
	isPostgres := strings.HasPrefix(dbConnStr, "postgres://") || strings.HasPrefix(dbConnStr, "postgresql://")

	r2AccountID := os.Getenv("R2_ACCOUNT_ID")
	r2AccessKeyID := os.Getenv("R2_ACCESS_KEY_ID")
	r2SecretAccessKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	r2BucketName := os.Getenv("R2_BUCKET_NAME")

	slog.Info("starting Apsthira")
	if isPostgres {
		slog.Info("config", "db", "PostgreSQL (URL masked)", "port", port)
	} else {
		slog.Info("config", "db", "SQLite", "db_path", dbConnStr, "port", port)
	}
	if r2AccountID == "" || r2AccessKeyID == "" || r2SecretAccessKey == "" || r2BucketName == "" {
		slog.Warn("Cloudflare R2 credentials not fully set — file uploads and downloads will fail")
	}

	database, err := db.InitDB(dbConnStr)
	if err != nil {
		slog.Error("database initialization failed", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	slog.Info("database connected", "engine", database.Driver())

	// Start background auto-sync worker if using SQLite locally and Supabase URL is set
	supabaseURL := os.Getenv("SUPABASE_DB_URL")
	if supabaseURL == "" {
		supabaseURL = os.Getenv("DATABASE_URL")
	}
	if supabaseURL != "" && !isPostgres {
		slog.Info("sync: starting background auto-sync worker (every 24 hours)")
		go func() {
			// Wait 5 seconds after server startup before running the first sync
			time.Sleep(5 * time.Second)
			slog.Info("sync: running startup auto-sync...")
			if err := db.RunAutoSync(dbConnStr, supabaseURL); err != nil {
				slog.Error("sync: startup auto-sync failed", "error", err)
			}

			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				slog.Info("sync: running scheduled daily auto-sync...")
				if err := db.RunAutoSync(dbConnStr, supabaseURL); err != nil {
					slog.Error("sync: scheduled auto-sync failed", "error", err)
				}
			}
		}()
	}

	var r2Client *storage.R2Client
	if r2AccountID != "" {
		ctx := context.Background()
		r2Client, err = storage.InitR2(ctx, r2AccountID, r2AccessKeyID, r2SecretAccessKey, r2BucketName)
		if err != nil {
			slog.Error("R2 client initialization failed", "error", err)
			os.Exit(1)
		}
	}

	tmpl, err := template.ParseFS(templatesFS,
		"templates/index.html",
		"templates/login.html",
		"templates/register.html",
		"templates/dashboard.html",
		"templates/view.html",
	)
	if err != nil {
		slog.Error("template parsing failed", "error", err)
		os.Exit(1)
	}

	h := handler.New(database, r2Client, tmpl)

	r := nanoserve.New()
	r.ErrorHandler = func(c *nanoserve.Context, err error) {
		slog.Error("unhandled request error", "error", err)
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Writer.WriteHeader(http.StatusInternalServerError)
	}

	r.Use(middleware.RequestLogger())
	r.Use(middleware.RateLimit(middleware.NewIPRateLimiter(rate.Limit(15), 30)))

	r.GET("/", h.LoadUser, h.HandleIndex)

	r.GET("/login", h.LoadUser, h.HandleLoginGet)
	r.POST("/login", h.LoadUser, h.HandleLoginPost)
	r.GET("/register", h.LoadUser, h.HandleRegisterGet)
	r.POST("/register", h.LoadUser, h.HandleRegisterPost)
	r.POST("/logout", h.HandleLogoutPost)
	r.POST("/delete-account", h.RequireAuth, h.HandleDeleteAccount)

	r.GET("/dashboard", h.RequireAuth, h.HandleDashboardGet)
	r.POST("/upload", h.RequireAuth, h.HandleUpload)
	r.POST("/r/:slug/update", h.RequireAuth, h.HandleUpdateResume)
	r.POST("/r/:slug/delete", h.RequireAuth, h.HandleDeleteResume)

	r.GET("/r/:slug", h.HandleViewResume)
	r.GET("/r/:slug/raw", h.HandleStreamResume)
	r.POST("/r/:slug/view", h.HandleIncrementViewCount)

	slog.Info("server listening", "url", "http://localhost:"+port)
	if err := r.Run(":" + port); err != nil {
		slog.Error("server listen failed", "error", err)
		os.Exit(1)
	}
}
