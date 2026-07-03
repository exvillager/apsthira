package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

// SyncSQLiteToPostgres replicates the entire local SQLite database to Supabase Postgres.
// It performs bulk upserts, deletes rows in Postgres that no longer exist in SQLite,
// and resets auto-increment sequences in Postgres.
func SyncSQLiteToPostgres(sqlitePath, postgresURL string) error {
	slog.Info("sync: starting database synchronization", "from", sqlitePath, "to", "Supabase Postgres")

	// 1. Connect to SQLite
	sqliteDB, err := sql.Open("sqlite3", sqlitePath)
	if err != nil {
		return fmt.Errorf("failed to open source sqlite db: %w", err)
	}
	defer sqliteDB.Close()

	// Verify sqlite connection
	if err = sqliteDB.Ping(); err != nil {
		return fmt.Errorf("failed to ping source sqlite db: %w", err)
	}

	// 2. Connect to Postgres
	postgresDB, err := sql.Open("postgres", postgresURL)
	if err != nil {
		return fmt.Errorf("failed to open target postgres db: %w", err)
	}
	defer postgresDB.Close()

	// Verify postgres connection
	if err = postgresDB.Ping(); err != nil {
		return fmt.Errorf("failed to ping target postgres db: %w", err)
	}

	// Start transaction on target database
	tx, err := postgresDB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin target transaction: %w", err)
	}
	defer tx.Rollback()

	// Ensure target schemas exist (run migrations on Postgres)
	// (Users, Resumes, and Sessions tables)
	schemaQuery := `
	CREATE TABLE IF NOT EXISTS users (
		id SERIAL PRIMARY KEY,
		username VARCHAR(255) UNIQUE NOT NULL,
		password_hash VARCHAR(255) NOT NULL,
		created_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS resumes (
		id SERIAL PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		slug VARCHAR(255) UNIQUE NOT NULL,
		r2_key VARCHAR(255) NOT NULL,
		original_filename VARCHAR(255) NOT NULL,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS sessions (
		token VARCHAR(255) PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		expires_at TIMESTAMP NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_resumes_slug ON resumes(slug);
	CREATE INDEX IF NOT EXISTS idx_resumes_user_id ON resumes(user_id);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username_lower ON users (LOWER(username));
	CREATE UNIQUE INDEX IF NOT EXISTS idx_resumes_slug_lower ON resumes (LOWER(slug));
	`
	if _, err = tx.Exec(schemaQuery); err != nil {
		return fmt.Errorf("failed to prepare target schemas: %w", err)
	}

	// 3. Sync Users
	slog.Info("sync: syncing 'users' table...")
	userRows, err := sqliteDB.Query("SELECT id, username, password_hash, created_at FROM users")
	if err != nil {
		return fmt.Errorf("failed to query source users: %w", err)
	}
	defer userRows.Close()

	var sqliteUserIDs []string
	userUpsertCount := 0
	for userRows.Next() {
		var id int64
		var username, passwordHash string
		var createdAt time.Time
		if err = userRows.Scan(&id, &username, &passwordHash, &createdAt); err != nil {
			return fmt.Errorf("failed to scan source user: %w", err)
		}
		sqliteUserIDs = append(sqliteUserIDs, strconv.FormatInt(id, 10))

		_, err = tx.Exec(`
			INSERT INTO users (id, username, password_hash, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO UPDATE SET
				username = EXCLUDED.username,
				password_hash = EXCLUDED.password_hash,
				created_at = EXCLUDED.created_at
		`, id, username, passwordHash, createdAt)
		if err != nil {
			return fmt.Errorf("failed to upsert user %d: %w", id, err)
		}
		userUpsertCount++
	}
	slog.Info("sync: upserted users", "count", userUpsertCount)

	// Delete users in target not in source
	if len(sqliteUserIDs) > 0 {
		delRes, err := tx.Exec("DELETE FROM users WHERE id NOT IN (" + strings.Join(sqliteUserIDs, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to delete orphaned users: %w", err)
		}
		delCount, _ := delRes.RowsAffected()
		if delCount > 0 {
			slog.Info("sync: deleted orphaned users", "count", delCount)
		}
	} else {
		// If SQLite users is completely empty, wipe target users
		delRes, err := tx.Exec("DELETE FROM users")
		if err != nil {
			return fmt.Errorf("failed to clear users: %w", err)
		}
		delCount, _ := delRes.RowsAffected()
		if delCount > 0 {
			slog.Info("sync: cleared users table", "count", delCount)
		}
	}

	// 4. Sync Resumes
	slog.Info("sync: syncing 'resumes' table...")
	resumeRows, err := sqliteDB.Query("SELECT id, user_id, slug, r2_key, original_filename, created_at, updated_at FROM resumes")
	if err != nil {
		return fmt.Errorf("failed to query source resumes: %w", err)
	}
	defer resumeRows.Close()

	var sqliteResumeIDs []string
	resumeUpsertCount := 0
	for resumeRows.Next() {
		var id, userID int64
		var slug, r2Key, originalFilename string
		var createdAt, updatedAt time.Time
		if err = resumeRows.Scan(&id, &userID, &slug, &r2Key, &originalFilename, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("failed to scan source resume: %w", err)
		}
		sqliteResumeIDs = append(sqliteResumeIDs, strconv.FormatInt(id, 10))

		_, err = tx.Exec(`
			INSERT INTO resumes (id, user_id, slug, r2_key, original_filename, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET
				user_id = EXCLUDED.user_id,
				slug = EXCLUDED.slug,
				r2_key = EXCLUDED.r2_key,
				original_filename = EXCLUDED.original_filename,
				created_at = EXCLUDED.created_at,
				updated_at = EXCLUDED.updated_at
		`, id, userID, slug, r2Key, originalFilename, createdAt, updatedAt)
		if err != nil {
			return fmt.Errorf("failed to upsert resume %d: %w", id, err)
		}
		resumeUpsertCount++
	}
	slog.Info("sync: upserted resumes", "count", resumeUpsertCount)

	// Delete resumes in target not in source
	if len(sqliteResumeIDs) > 0 {
		delRes, err := tx.Exec("DELETE FROM resumes WHERE id NOT IN (" + strings.Join(sqliteResumeIDs, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to delete orphaned resumes: %w", err)
		}
		delCount, _ := delRes.RowsAffected()
		if delCount > 0 {
			slog.Info("sync: deleted orphaned resumes", "count", delCount)
		}
	} else {
		_, err = tx.Exec("DELETE FROM resumes")
		if err != nil {
			return fmt.Errorf("failed to clear resumes: %w", err)
		}
	}

	// 5. Sync Sessions
	slog.Info("sync: syncing 'sessions' table...")
	sessionRows, err := sqliteDB.Query("SELECT token, user_id, expires_at FROM sessions")
	if err != nil {
		return fmt.Errorf("failed to query source sessions: %w", err)
	}
	defer sessionRows.Close()

	var sqliteSessionTokens []string
	sessionUpsertCount := 0
	for sessionRows.Next() {
		var token string
		var userID int64
		var expiresAt time.Time
		if err = sessionRows.Scan(&token, &userID, &expiresAt); err != nil {
			return fmt.Errorf("failed to scan source session: %w", err)
		}
		// Wrap tokens in single quotes for query insertion safely
		sqliteSessionTokens = append(sqliteSessionTokens, fmt.Sprintf("'%s'", strings.ReplaceAll(token, "'", "''")))

		_, err = tx.Exec(`
			INSERT INTO sessions (token, user_id, expires_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (token) DO UPDATE SET
				user_id = EXCLUDED.user_id,
				expires_at = EXCLUDED.expires_at
		`, token, userID, expiresAt)
		if err != nil {
			return fmt.Errorf("failed to upsert session token: %w", err)
		}
		sessionUpsertCount++
	}
	slog.Info("sync: upserted sessions", "count", sessionUpsertCount)

	// Delete sessions in target not in source
	if len(sqliteSessionTokens) > 0 {
		delRes, err := tx.Exec("DELETE FROM sessions WHERE token NOT IN (" + strings.Join(sqliteSessionTokens, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to delete orphaned sessions: %w", err)
		}
		delCount, _ := delRes.RowsAffected()
		if delCount > 0 {
			slog.Info("sync: deleted orphaned sessions", "count", delCount)
		}
	} else {
		_, err = tx.Exec("DELETE FROM sessions")
		if err != nil {
			return fmt.Errorf("failed to clear sessions: %w", err)
		}
	}

	// 6. Reset postgres sequence values to match the newly inserted IDs
	slog.Info("sync: resetting postgres sequence generators...")
	_, err = tx.Exec(`SELECT setval('users_id_seq', COALESCE((SELECT MAX(id) FROM users), 1))`)
	if err != nil {
		return fmt.Errorf("failed to reset users sequence: %w", err)
	}
	_, err = tx.Exec(`SELECT setval('resumes_id_seq', COALESCE((SELECT MAX(id) FROM resumes), 1))`)
	if err != nil {
		return fmt.Errorf("failed to reset resumes sequence: %w", err)
	}

	// Commit transaction
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit sync transaction: %w", err)
	}

	slog.Info("sync: database synchronization completed successfully!")
	return nil
}
