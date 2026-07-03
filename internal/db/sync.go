package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

// ensureSQLiteSchema runs standard migrations on the local SQLite DB to make sure tables exist.
func ensureSQLiteSchema(db *sql.DB) error {
	query := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS resumes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		slug TEXT UNIQUE NOT NULL,
		r2_key TEXT NOT NULL,
		original_filename TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		FOREIGN KEY(user_id) REFERENCES users(id)
	);

	CREATE TABLE IF NOT EXISTS sessions (
		token TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		expires_at DATETIME NOT NULL,
		FOREIGN KEY(user_id) REFERENCES users(id)
	);

	CREATE INDEX IF NOT EXISTS idx_resumes_slug ON resumes(slug);
	CREATE INDEX IF NOT EXISTS idx_resumes_user_id ON resumes(user_id);
	`
	_, err := db.Exec(query)
	return err
}

// SyncSQLiteToPostgres replicates the entire local SQLite database to Supabase Postgres (Push).
// It has a safety check to prevent accidental deletion of remote data if the local SQLite DB is empty.
func SyncSQLiteToPostgres(sqlitePath, postgresURL string) error {
	slog.Info("sync-push: starting database synchronization (SQLite -> Supabase)", "from", sqlitePath)

	// 1. Connect to SQLite
	sqliteDB, err := sql.Open("sqlite3", sqlitePath)
	if err != nil {
		return fmt.Errorf("failed to open source sqlite db: %w", err)
	}
	defer sqliteDB.Close()

	if err = sqliteDB.Ping(); err != nil {
		return fmt.Errorf("failed to ping source sqlite db: %w", err)
	}

	// Run SQLite migrations to make sure tables exist
	if err = ensureSQLiteSchema(sqliteDB); err != nil {
		return fmt.Errorf("failed to initialize sqlite schema: %w", err)
	}

	// 2. Connect to Postgres
	postgresDB, err := sql.Open("postgres", postgresURL)
	if err != nil {
		return fmt.Errorf("failed to open target postgres db: %w", err)
	}
	defer postgresDB.Close()

	if err = postgresDB.Ping(); err != nil {
		return fmt.Errorf("failed to ping target postgres db: %w", err)
	}

	// 3. Safety Check: If local SQLite is empty but Supabase is not, abort to prevent data wipe!
	var sqliteUserCount int
	err = sqliteDB.QueryRow("SELECT COUNT(*) FROM users").Scan(&sqliteUserCount)
	if err != nil {
		return fmt.Errorf("failed to count source users: %w", err)
	}

	var pgUserCount int
	// We run schema creation on Postgres first if tables don't exist
	schemaQuery := `
	CREATE TABLE IF NOT EXISTS users (
		id SERIAL PRIMARY KEY,
		username VARCHAR(255) UNIQUE NOT NULL,
		password_hash VARCHAR(255) NOT NULL,
		created_at TIMESTAMP NOT NULL
	);
	`
	if _, err = postgresDB.Exec(schemaQuery); err != nil {
		return fmt.Errorf("failed to verify target users table: %w", err)
	}

	err = postgresDB.QueryRow("SELECT COUNT(*) FROM users").Scan(&pgUserCount)
	if err != nil {
		// If query fails for another reason, log it
		slog.Warn("sync-push: could not count target users", "error", err)
	}

	if sqliteUserCount == 0 && pgUserCount > 0 {
		slog.Error("SAFETY BLOCK TRIGGERED: Your local SQLite database is completely empty, but Supabase Postgres has active users. Running a push sync would delete all Supabase data.")
		slog.Error("To bootstrap/initialize your local SQLite database from Supabase, run this first:")
		slog.Error("    ./apsthira --sync-pull")
		return errors.New("aborted push sync to prevent remote database wipe")
	}

	// Start transaction on Postgres
	tx, err := postgresDB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin target transaction: %w", err)
	}
	defer tx.Rollback()

	// Ensure all target schemas exist on Postgres
	fullSchemaQuery := `
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
	if _, err = tx.Exec(fullSchemaQuery); err != nil {
		return fmt.Errorf("failed to prepare target schemas: %w", err)
	}

	// 4. Sync Users
	slog.Info("sync-push: syncing 'users' table...")
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
	slog.Info("sync-push: upserted users", "count", userUpsertCount)

	// Delete users in target not in source
	if len(sqliteUserIDs) > 0 {
		delRes, err := tx.Exec("DELETE FROM users WHERE id NOT IN (" + strings.Join(sqliteUserIDs, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to delete orphaned users: %w", err)
		}
		delCount, _ := delRes.RowsAffected()
		if delCount > 0 {
			slog.Info("sync-push: deleted orphaned users", "count", delCount)
		}
	} else {
		// Safe only because we checked user count earlier
		_, _ = tx.Exec("DELETE FROM users")
	}

	// 5. Sync Resumes
	slog.Info("sync-push: syncing 'resumes' table...")
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
	slog.Info("sync-push: upserted resumes", "count", resumeUpsertCount)

	// Delete resumes in target not in source
	if len(sqliteResumeIDs) > 0 {
		delRes, err := tx.Exec("DELETE FROM resumes WHERE id NOT IN (" + strings.Join(sqliteResumeIDs, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to delete orphaned resumes: %w", err)
		}
		delCount, _ := delRes.RowsAffected()
		if delCount > 0 {
			slog.Info("sync-push: deleted orphaned resumes", "count", delCount)
		}
	} else {
		_, _ = tx.Exec("DELETE FROM resumes")
	}

	// 6. Sync Sessions
	slog.Info("sync-push: syncing 'sessions' table...")
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
	slog.Info("sync-push: upserted sessions", "count", sessionUpsertCount)

	// Delete sessions in target not in source
	if len(sqliteSessionTokens) > 0 {
		delRes, err := tx.Exec("DELETE FROM sessions WHERE token NOT IN (" + strings.Join(sqliteSessionTokens, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to delete orphaned sessions: %w", err)
		}
		delCount, _ := delRes.RowsAffected()
		if delCount > 0 {
			slog.Info("sync-push: deleted orphaned sessions", "count", delCount)
		}
	} else {
		_, _ = tx.Exec("DELETE FROM sessions")
	}

	// 7. Reset Postgres sequences
	slog.Info("sync-push: resetting postgres sequence generators...")
	_, err = tx.Exec(`SELECT setval('users_id_seq', COALESCE((SELECT MAX(id) FROM users), 1))`)
	if err != nil {
		return fmt.Errorf("failed to reset users sequence: %w", err)
	}
	_, err = tx.Exec(`SELECT setval('resumes_id_seq', COALESCE((SELECT MAX(id) FROM resumes), 1))`)
	if err != nil {
		return fmt.Errorf("failed to reset resumes sequence: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit Postgres transaction: %w", err)
	}

	slog.Info("sync-push: database synchronization completed successfully!")
	return nil
}

// SyncPostgresToSQLite replicates the entire remote Supabase Postgres database to local SQLite (Pull).
// This is used for initialization/bootstrapping or recovering local databases.
func SyncPostgresToSQLite(sqlitePath, postgresURL string) error {
	slog.Info("sync-pull: starting database synchronization (Supabase -> SQLite)", "to", sqlitePath)

	// 1. Connect to SQLite
	sqliteDB, err := sql.Open("sqlite3", sqlitePath)
	if err != nil {
		return fmt.Errorf("failed to open sqlite target: %w", err)
	}
	defer sqliteDB.Close()

	if err = sqliteDB.Ping(); err != nil {
		return fmt.Errorf("failed to ping sqlite target: %w", err)
	}

	// Ensure SQLite schema exists
	if err = ensureSQLiteSchema(sqliteDB); err != nil {
		return fmt.Errorf("failed to initialize sqlite schema: %w", err)
	}

	// 2. Connect to Postgres
	postgresDB, err := sql.Open("postgres", postgresURL)
	if err != nil {
		return fmt.Errorf("failed to open source postgres db: %w", err)
	}
	defer postgresDB.Close()

	if err = postgresDB.Ping(); err != nil {
		return fmt.Errorf("failed to ping source postgres db: %w", err)
	}

	// Start SQLite transaction
	tx, err := sqliteDB.Begin()
	if err != nil {
		return fmt.Errorf("failed to start SQLite transaction: %w", err)
	}
	defer tx.Rollback()

	// 3. Sync Users
	slog.Info("sync-pull: fetching users from Supabase...")
	userRows, err := postgresDB.Query("SELECT id, username, password_hash, created_at FROM users")
	if err != nil {
		return fmt.Errorf("failed to query source users from Postgres: %w", err)
	}
	defer userRows.Close()

	var pgUserIDs []string
	userCount := 0
	for userRows.Next() {
		var id int64
		var username, passwordHash string
		var createdAt time.Time
		if err = userRows.Scan(&id, &username, &passwordHash, &createdAt); err != nil {
			return fmt.Errorf("failed to scan source user: %w", err)
		}
		pgUserIDs = append(pgUserIDs, strconv.FormatInt(id, 10))

		_, err = tx.Exec(`
			INSERT INTO users (id, username, password_hash, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				username = excluded.username,
				password_hash = excluded.password_hash,
				created_at = excluded.created_at
		`, id, username, passwordHash, createdAt)
		if err != nil {
			return fmt.Errorf("failed to upsert SQLite user %d: %w", id, err)
		}
		userCount++
	}
	slog.Info("sync-pull: stored users in SQLite", "count", userCount)

	// Delete users in SQLite not in Postgres
	if len(pgUserIDs) > 0 {
		_, err = tx.Exec("DELETE FROM users WHERE id NOT IN (" + strings.Join(pgUserIDs, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to clean SQLite users: %w", err)
		}
	} else {
		_, _ = tx.Exec("DELETE FROM users")
	}

	// 4. Sync Resumes
	slog.Info("sync-pull: fetching resumes from Supabase...")
	resumeRows, err := postgresDB.Query("SELECT id, user_id, slug, r2_key, original_filename, created_at, updated_at FROM resumes")
	if err != nil {
		return fmt.Errorf("failed to query source resumes from Postgres: %w", err)
	}
	defer resumeRows.Close()

	var pgResumeIDs []string
	resumeCount := 0
	for resumeRows.Next() {
		var id, userID int64
		var slug, r2Key, originalFilename string
		var createdAt, updatedAt time.Time
		if err = resumeRows.Scan(&id, &userID, &slug, &r2Key, &originalFilename, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("failed to scan source resume: %w", err)
		}
		pgResumeIDs = append(pgResumeIDs, strconv.FormatInt(id, 10))

		_, err = tx.Exec(`
			INSERT INTO resumes (id, user_id, slug, r2_key, original_filename, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				user_id = excluded.user_id,
				slug = excluded.slug,
				r2_key = excluded.r2_key,
				original_filename = excluded.original_filename,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at
		`, id, userID, slug, r2Key, originalFilename, createdAt, updatedAt)
		if err != nil {
			return fmt.Errorf("failed to upsert SQLite resume %d: %w", id, err)
		}
		resumeCount++
	}
	slog.Info("sync-pull: stored resumes in SQLite", "count", resumeCount)

	// Delete resumes in SQLite not in Postgres
	if len(pgResumeIDs) > 0 {
		_, err = tx.Exec("DELETE FROM resumes WHERE id NOT IN (" + strings.Join(pgResumeIDs, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to clean SQLite resumes: %w", err)
		}
	} else {
		_, _ = tx.Exec("DELETE FROM resumes")
	}

	// 5. Sync Sessions
	slog.Info("sync-pull: fetching sessions from Supabase...")
	sessionRows, err := postgresDB.Query("SELECT token, user_id, expires_at FROM sessions")
	if err != nil {
		return fmt.Errorf("failed to query source sessions from Postgres: %w", err)
	}
	defer sessionRows.Close()

	var pgSessionTokens []string
	sessionCount := 0
	for sessionRows.Next() {
		var token string
		var userID int64
		var expiresAt time.Time
		if err = sessionRows.Scan(&token, &userID, &expiresAt); err != nil {
			return fmt.Errorf("failed to scan source session: %w", err)
		}
		pgSessionTokens = append(pgSessionTokens, fmt.Sprintf("'%s'", strings.ReplaceAll(token, "'", "''")))

		_, err = tx.Exec(`
			INSERT INTO sessions (token, user_id, expires_at)
			VALUES (?, ?, ?)
			ON CONFLICT(token) DO UPDATE SET
				user_id = excluded.user_id,
				expires_at = excluded.expires_at
		`, token, userID, expiresAt)
		if err != nil {
			return fmt.Errorf("failed to upsert SQLite session: %w", err)
		}
		sessionCount++
	}
	slog.Info("sync-pull: stored sessions in SQLite", "count", sessionCount)

	// Delete sessions in SQLite not in Postgres
	if len(pgSessionTokens) > 0 {
		_, err = tx.Exec("DELETE FROM sessions WHERE token NOT IN (" + strings.Join(pgSessionTokens, ",") + ")")
		if err != nil {
			return fmt.Errorf("failed to clean SQLite sessions: %w", err)
		}
	} else {
		_, _ = tx.Exec("DELETE FROM sessions")
	}

	// Commit SQLite transaction
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit SQLite transaction: %w", err)
	}

	slog.Info("sync-pull: database pull synchronization completed successfully!")
	return nil
}
