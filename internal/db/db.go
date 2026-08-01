package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

type Resume struct {
	ID               int64
	UserID           int64
	Slug             string
	R2Key            string
	OriginalFilename string
	ViewsCount       int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Session struct {
	Token     string
	UserID    int64
	ExpiresAt time.Time
}

type DB struct {
	conn   *sql.DB
	driver string
}

func CleanPostgresURL(url string) string {
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		if !strings.Contains(url, "binary_parameters=") {
			if strings.Contains(url, "?") {
				return url + "&binary_parameters=yes"
			} else {
				return url + "?binary_parameters=yes"
			}
		}
	}
	return url
}

func InitDB(connStr string) (*DB, error) {
	var driver string
	var conn *sql.DB
	var err error

	// Detect driver based on connection string prefix
	if strings.HasPrefix(connStr, "postgres://") || strings.HasPrefix(connStr, "postgresql://") {
		driver = "postgres"
		connStr = CleanPostgresURL(connStr)
		conn, err = sql.Open("postgres", connStr)
	} else {
		driver = "sqlite3"
		conn, err = sql.Open("sqlite3", connStr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open database connection: %w", err)
	}

	db := &DB{conn: conn, driver: driver}

	// Create tables depending on database driver
	var query string
	if driver == "postgres" {
		query = `
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
			views_count INTEGER DEFAULT 0 NOT NULL,
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
		`
	} else {
		query = `
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
			views_count INTEGER DEFAULT 0 NOT NULL,
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
	}

	if _, err := conn.Exec(query); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	// Run incremental migration for views_count column on existing databases
	if driver == "postgres" {
		_, _ = conn.Exec(`ALTER TABLE resumes ADD COLUMN IF NOT EXISTS views_count INTEGER DEFAULT 0 NOT NULL`)
	} else {
		_, _ = conn.Exec(`ALTER TABLE resumes ADD COLUMN views_count INTEGER DEFAULT 0 NOT NULL`)
	}

	// Try to create case-insensitive unique indexes to enforce uniqueness at the DB level.
	// Since there might be existing duplicates, we log a warning but don't fail startup.
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username_lower ON users (LOWER(username))`)
	_, _ = conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_resumes_slug_lower ON resumes (LOWER(slug))`)

	return db, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) Driver() string {
	return db.driver
}

func (db *DB) Ping() error {
	return db.conn.Ping()
}

// q replaces placeholder '?' with '$1, $2...' for PostgreSQL
func (db *DB) q(query string) string {
	if db.driver == "postgres" {
		n := 1
		for {
			if !strings.Contains(query, "?") {
				break
			}
			query = strings.Replace(query, "?", fmt.Sprintf("$%d", n), 1)
			n++
		}
	}
	return query
}

// User Helpers
func (db *DB) CreateUser(username, passwordHash string) (int64, error) {
	now := time.Now()
	if db.driver == "postgres" {
		query := `INSERT INTO users (username, password_hash, created_at) VALUES ($1, $2, $3) RETURNING id`
		var id int64
		err := db.conn.QueryRow(query, username, passwordHash, now).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("failed to create user: %w", err)
		}
		return id, nil
	} else {
		query := `INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)`
		res, err := db.conn.Exec(query, username, passwordHash, now)
		if err != nil {
			return 0, fmt.Errorf("failed to create user: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		return id, nil
	}
}

func (db *DB) GetUserByUsername(username string) (*User, error) {
	query := db.q(`SELECT id, username, password_hash, created_at FROM users WHERE LOWER(username) = LOWER(?)`)
	row := db.conn.QueryRow(query, username)
	var u User
	var createdAtVal any
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAtVal)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan user: %w", err)
	}

	// Parse timestamps
	u.CreatedAt = db.parseTime(createdAtVal)
	return &u, nil
}

func (db *DB) GetUserByID(id int64) (*User, error) {
	query := db.q(`SELECT id, username, password_hash, created_at FROM users WHERE id = ?`)
	row := db.conn.QueryRow(query, id)
	var u User
	var createdAtVal any
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAtVal)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan user: %w", err)
	}

	u.CreatedAt = db.parseTime(createdAtVal)
	return &u, nil
}

// Session Helpers
func (db *DB) CreateSession(token string, userID int64, expiresAt time.Time) error {
	query := db.q(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`)
	_, err := db.conn.Exec(query, token, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

func (db *DB) GetSession(token string) (*Session, error) {
	query := db.q(`SELECT token, user_id, expires_at FROM sessions WHERE token = ?`)
	row := db.conn.QueryRow(query, token)
	var s Session
	var expiresAtVal any
	err := row.Scan(&s.Token, &s.UserID, &expiresAtVal)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan session: %w", err)
	}

	s.ExpiresAt = db.parseTime(expiresAtVal)
	return &s, nil
}

// GetSessionWithUser fetches session + user in a single JOIN instead of two queries.
func (db *DB) GetSessionWithUser(token string) (*User, *Session, error) {
	query := db.q(`
	SELECT u.id, u.username, u.password_hash, u.created_at, s.expires_at
	FROM sessions s
	JOIN users u ON u.id = s.user_id
	WHERE s.token = ?
	`)
	row := db.conn.QueryRow(query, token)
	var u User
	var s Session
	var createdAtVal, expiresAtVal any
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAtVal, &expiresAtVal)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("failed to scan session+user: %w", err)
	}
	u.CreatedAt = db.parseTime(createdAtVal)
	s.Token = token
	s.UserID = u.ID
	s.ExpiresAt = db.parseTime(expiresAtVal)
	return &u, &s, nil
}

func (db *DB) DeleteSession(token string) error {
	query := db.q(`DELETE FROM sessions WHERE token = ?`)
	_, err := db.conn.Exec(query, token)
	return err
}

// Resume Helpers
func (db *DB) CreateResume(userID int64, slug, r2Key, originalFilename string) error {
	query := db.q(`
	INSERT INTO resumes (user_id, slug, r2_key, original_filename, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	`)
	now := time.Now()
	_, err := db.conn.Exec(query, userID, slug, r2Key, originalFilename, now, now)
	if err != nil {
		return fmt.Errorf("failed to insert resume record: %w", err)
	}
	return nil
}

func (db *DB) GetResume(slug string) (*Resume, error) {
	query := db.q(`
	SELECT id, user_id, slug, r2_key, original_filename, views_count, created_at, updated_at
	FROM resumes
	WHERE LOWER(slug) = LOWER(?)
	`)
	row := db.conn.QueryRow(query, slug)
	var r Resume
	var createdAtVal, updatedAtVal any
	err := row.Scan(&r.ID, &r.UserID, &r.Slug, &r.R2Key, &r.OriginalFilename, &r.ViewsCount, &createdAtVal, &updatedAtVal)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to query resume: %w", err)
	}

	r.CreatedAt = db.parseTime(createdAtVal)
	r.UpdatedAt = db.parseTime(updatedAtVal)

	return &r, nil
}

func (db *DB) GetResumesByUserID(userID int64) ([]Resume, error) {
	query := db.q(`
	SELECT id, user_id, slug, r2_key, original_filename, views_count, created_at, updated_at
	FROM resumes
	WHERE user_id = ?
	ORDER BY updated_at DESC
	`)
	rows, err := db.conn.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Resume
	for rows.Next() {
		var r Resume
		var createdAtVal, updatedAtVal any
		err := rows.Scan(&r.ID, &r.UserID, &r.Slug, &r.R2Key, &r.OriginalFilename, &r.ViewsCount, &createdAtVal, &updatedAtVal)
		if err != nil {
			return nil, err
		}
		r.CreatedAt = db.parseTime(createdAtVal)
		r.UpdatedAt = db.parseTime(updatedAtVal)
		list = append(list, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

func (db *DB) UpdateResume(slug, r2Key, originalFilename string) error {
	query := db.q(`
	UPDATE resumes
	SET r2_key = ?, original_filename = ?, updated_at = ?
	WHERE LOWER(slug) = LOWER(?)
	`)
	_, err := db.conn.Exec(query, r2Key, originalFilename, time.Now(), slug)
	if err != nil {
		return fmt.Errorf("failed to update resume record: %w", err)
	}
	return nil
}

func (db *DB) DeleteResume(slug string) error {
	query := db.q(`DELETE FROM resumes WHERE LOWER(slug) = LOWER(?)`)
	_, err := db.conn.Exec(query, slug)
	return err
}

func (db *DB) IncrementViews(slug string) error {
	query := db.q(`UPDATE resumes SET views_count = views_count + 1 WHERE LOWER(slug) = LOWER(?)`)
	_, err := db.conn.Exec(query, slug)
	if err != nil {
		return fmt.Errorf("failed to increment views count: %w", err)
	}
	return nil
}

func (db *DB) DeleteUserAndResources(userID int64) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. Delete all resumes associated with the user
	queryResumes := db.q(`DELETE FROM resumes WHERE user_id = ?`)
	_, err = tx.Exec(queryResumes, userID)
	if err != nil {
		return err
	}

	// 2. Delete all sessions associated with the user
	querySessions := db.q(`DELETE FROM sessions WHERE user_id = ?`)
	_, err = tx.Exec(querySessions, userID)
	if err != nil {
		return err
	}

	// 3. Delete the user
	queryUser := db.q(`DELETE FROM users WHERE id = ?`)
	_, err = tx.Exec(queryUser, userID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// parseTime formats datetime string or direct time.Time from SQLite or PostgreSQL driver formats safely
func (db *DB) parseTime(val any) time.Time {
	if val == nil {
		return time.Time{}
	}
	if t, ok := val.(time.Time); ok {
		return t
	}
	var tStr string
	if b, ok := val.([]byte); ok {
		tStr = string(b)
	} else if s, ok := val.(string); ok {
		tStr = s
	} else {
		return time.Now()
	}

	tStr = strings.TrimSpace(tStr)
	
	// Standard layouts
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999 -0700",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05-0700",
		"2006-01-02 15:04:05-07",
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
	}
	
	for _, layout := range layouts {
		if parsedVal, err := time.Parse(layout, tStr); err == nil {
			return parsedVal
		}
	}
	return time.Now() // default fallback
}
