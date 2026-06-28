package main

import (
	"database/sql"
	"fmt"
	"time"

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
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Session struct {
	Token     string
	UserID    int64
	ExpiresAt time.Time
}

type DB struct {
	conn *sql.DB
}

func InitDB(dbPath string) (*DB, error) {
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Create tables
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
	if _, err := conn.Exec(query); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

// User Helpers
func (db *DB) CreateUser(username, passwordHash string) (int64, error) {
	query := `INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)`
	now := time.Now()
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

func (db *DB) GetUserByUsername(username string) (*User, error) {
	query := `SELECT id, username, password_hash, created_at FROM users WHERE username = ?`
	row := db.conn.QueryRow(query, username)
	var u User
	var createdAtStr string
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan user: %w", err)
	}

	u.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
	if u.CreatedAt.IsZero() {
		u.CreatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", createdAtStr)
	}
	return &u, nil
}

func (db *DB) GetUserByID(id int64) (*User, error) {
	query := `SELECT id, username, password_hash, created_at FROM users WHERE id = ?`
	row := db.conn.QueryRow(query, id)
	var u User
	var createdAtStr string
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &createdAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan user: %w", err)
	}

	u.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
	if u.CreatedAt.IsZero() {
		u.CreatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", createdAtStr)
	}
	return &u, nil
}

// Session Helpers
func (db *DB) CreateSession(token string, userID int64, expiresAt time.Time) error {
	query := `INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`
	_, err := db.conn.Exec(query, token, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

func (db *DB) GetSession(token string) (*Session, error) {
	query := `SELECT token, user_id, expires_at FROM sessions WHERE token = ?`
	row := db.conn.QueryRow(query, token)
	var s Session
	var expiresAtStr string
	err := row.Scan(&s.Token, &s.UserID, &expiresAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to scan session: %w", err)
	}

	s.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAtStr)
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", expiresAtStr)
	}
	return &s, nil
}

func (db *DB) DeleteSession(token string) error {
	query := `DELETE FROM sessions WHERE token = ?`
	_, err := db.conn.Exec(query, token)
	return err
}

// Resume Helpers
func (db *DB) CreateResume(userID int64, slug, r2Key, originalFilename string) error {
	query := `
	INSERT INTO resumes (user_id, slug, r2_key, original_filename, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	`
	now := time.Now()
	_, err := db.conn.Exec(query, userID, slug, r2Key, originalFilename, now, now)
	if err != nil {
		return fmt.Errorf("failed to insert resume record: %w", err)
	}
	return nil
}

func (db *DB) GetResume(slug string) (*Resume, error) {
	query := `
	SELECT id, user_id, slug, r2_key, original_filename, created_at, updated_at
	FROM resumes
	WHERE slug = ?
	`
	row := db.conn.QueryRow(query, slug)
	var r Resume
	var createdAtStr, updatedAtStr string
	err := row.Scan(&r.ID, &r.UserID, &r.Slug, &r.R2Key, &r.OriginalFilename, &createdAtStr, &updatedAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to query resume: %w", err)
	}

	r.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
	if r.CreatedAt.IsZero() {
		r.CreatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", createdAtStr)
	}
	r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", updatedAtStr)
	}

	return &r, nil
}

func (db *DB) GetResumesByUserID(userID int64) ([]Resume, error) {
	query := `
	SELECT id, user_id, slug, r2_key, original_filename, created_at, updated_at
	FROM resumes
	WHERE user_id = ?
	ORDER BY updated_at DESC
	`
	rows, err := db.conn.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Resume
	for rows.Next() {
		var r Resume
		var createdAtStr, updatedAtStr string
		err := rows.Scan(&r.ID, &r.UserID, &r.Slug, &r.R2Key, &r.OriginalFilename, &createdAtStr, &updatedAtStr)
		if err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)
		if r.CreatedAt.IsZero() {
			r.CreatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", createdAtStr)
		}
		r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)
		if r.UpdatedAt.IsZero() {
			r.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", updatedAtStr)
		}
		list = append(list, r)
	}
	return list, nil
}

func (db *DB) UpdateResume(slug, r2Key, originalFilename string) error {
	query := `
	UPDATE resumes
	SET r2_key = ?, original_filename = ?, updated_at = ?
	WHERE slug = ?
	`
	_, err := db.conn.Exec(query, r2Key, originalFilename, time.Now(), slug)
	if err != nil {
		return fmt.Errorf("failed to update resume record: %w", err)
	}
	return nil
}

func (db *DB) DeleteResume(slug string) error {
	query := `DELETE FROM resumes WHERE slug = ?`
	_, err := db.conn.Exec(query, slug)
	return err
}
