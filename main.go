package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/exvillager/nanoserve"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*
var templatesFS embed.FS

var (
	db        *DB
	r2Client  *R2Client
	tmpl      *template.Template
	slugRegex = regexp.MustCompile(`^[a-zA-Z0-9-_]{3,30}$`)
)

func isValidSlug(slug string) bool {
	return slugRegex.MatchString(slug)
}

func main() {
	// Load .env file
	_ = godotenv.Load()

	// 1. Load Configurations
	port := getEnv("PORT", "8080")
	dbPath := getEnv("DB_PATH", "resumes.db")

	r2AccountID := os.Getenv("R2_ACCOUNT_ID")
	r2AccessKeyID := os.Getenv("R2_ACCESS_KEY_ID")
	r2SecretAccessKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	r2BucketName := os.Getenv("R2_BUCKET_NAME")

	log.Printf("Starting Apsthira with Login Authentication (nanoServe router)...")
	log.Printf("Config - DB Path: %s, Port: %s", dbPath, port)
	log.Printf("Config - R2 Bucket: %s, Account ID: %s", r2BucketName, r2AccountID)

	if r2AccountID == "" || r2AccessKeyID == "" || r2SecretAccessKey == "" || r2BucketName == "" {
		log.Println("WARNING: Cloudflare R2 credentials are not fully set. File uploads and downloads will fail.")
	}

	// 2. Initialize SQLite Database
	var err error
	db, err = InitDB(dbPath)
	if err != nil {
		log.Fatalf("Database initialization failed: %v", err)
	}
	defer db.Close()

	// 3. Initialize Cloudflare R2 Client
	ctx := context.Background()
	if r2AccountID != "" {
		r2Client, err = InitR2(ctx, r2AccountID, r2AccessKeyID, r2SecretAccessKey, r2BucketName)
		if err != nil {
			log.Fatalf("R2 client initialization failed: %v", err)
		}
	}

	// 4. Parse Templates
	tmpl, err = template.ParseFS(templatesFS,
		"templates/index.html",
		"templates/login.html",
		"templates/register.html",
		"templates/dashboard.html",
		"templates/view.html",
	)
	if err != nil {
		log.Fatalf("Template parsing failed: %v", err)
	}

	// 5. Initialize nanoServe Router
	r := nanoserve.New()

	r.ErrorHandler = func(c *nanoserve.Context, err error) {
		log.Printf("Request error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, err.Error())
	}

	// Router mappings
	r.GET("/", handleIndex)
	
	// Auth Routes
	r.GET("/login", handleLoginGet)
	r.POST("/login", handleLoginPost)
	r.GET("/register", handleRegisterGet)
	r.POST("/register", handleRegisterPost)
	r.POST("/logout", handleLogoutPost)
	r.POST("/delete-account", handleDeleteAccount)
	
	// Dashboard & Admin Routes
	r.GET("/dashboard", handleDashboardGet)
	r.POST("/upload", handleUpload)
	r.POST("/r/:slug/update", handleUpdateResume)
	r.POST("/r/:slug/delete", handleDeleteResume)
	
	// Public Routes
	r.GET("/r/:slug", handleViewResume)
	r.GET("/r/:slug/raw", handleStreamResume)

	log.Printf("Server listening on http://localhost:%s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("Server listen failed: %v", err)
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// Write JSON error response helper
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// Session authentication helpers
func generateSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func getLoggedInUser(r *http.Request) *User {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return nil
	}
	session, err := db.GetSession(cookie.Value)
	if err != nil || session == nil {
		return nil
	}
	if session.ExpiresAt.Before(time.Now()) {
		_ = db.DeleteSession(session.Token)
		return nil
	}
	
	user, err := db.GetUserByID(session.UserID)
	if err != nil {
		return nil
	}
	return user
}

// Handler: Landing / index page
func handleIndex(c *nanoserve.Context) error {
	user := getLoggedInUser(c.Request)
	if user != nil {
		// Already logged in, redirect to dashboard
		c.Redirect("/dashboard", http.StatusSeeOther)
		return nil
	}

	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(c.Writer, "index.html", nil)
}

// Handler: GET /login
func handleLoginGet(c *nanoserve.Context) error {
	user := getLoggedInUser(c.Request)
	if user != nil {
		c.Redirect("/dashboard", http.StatusSeeOther)
		return nil
	}

	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(c.Writer, "login.html", nil)
}

// Handler: POST /login
func handleLoginPost(c *nanoserve.Context) error {
	username := strings.TrimSpace(c.Request.FormValue("username"))
	password := c.Request.FormValue("password")

	data := map[string]interface{}{}

	if username == "" || password == "" {
		data["Error"] = "Username and password are required."
		c.Status(http.StatusBadRequest)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	user, err := db.GetUserByUsername(username)
	if err != nil {
		log.Printf("Login lookup error: %v", err)
		data["Error"] = "Internal database error."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	if user == nil {
		data["Error"] = "Invalid username or password."
		c.Status(http.StatusUnauthorized)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	// Compare password hash
	err = bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password))
	if err != nil {
		data["Error"] = "Invalid username or password."
		c.Status(http.StatusUnauthorized)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	// Create session
	token := generateSessionToken()
	expires := time.Now().Add(24 * time.Hour) // 1 day session
	err = db.CreateSession(token, user.ID, expires)
	if err != nil {
		log.Printf("Session creation error: %v", err)
		data["Error"] = "Failed to initiate session."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	// Set cookie
	c.SetCookie(http.Cookie{
		Name:     "session_token",
		Value:    token,
		Expires:  expires,
		HttpOnly: true,
		Path:     "/",
	})

	c.Redirect("/dashboard", http.StatusSeeOther)
	return nil
}

// Handler: GET /register
func handleRegisterGet(c *nanoserve.Context) error {
	user := getLoggedInUser(c.Request)
	if user != nil {
		c.Redirect("/dashboard", http.StatusSeeOther)
		return nil
	}

	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(c.Writer, "register.html", nil)
}

// Handler: POST /register
func handleRegisterPost(c *nanoserve.Context) error {
	username := strings.TrimSpace(c.Request.FormValue("username"))
	password := c.Request.FormValue("password")

	data := map[string]interface{}{}

	if username == "" || password == "" {
		data["Error"] = "Username and password are required."
		c.Status(http.StatusBadRequest)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	if len(username) < 3 || len(password) < 6 {
		data["Error"] = "Username must be at least 3 chars and password at least 6 chars."
		c.Status(http.StatusBadRequest)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	// Check if username taken
	existing, err := db.GetUserByUsername(username)
	if err != nil {
		log.Printf("Registration username check error: %v", err)
		data["Error"] = "Database lookup failed."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	if existing != nil {
		data["Error"] = "Username is already taken."
		c.Status(http.StatusConflict)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	// Hash password
	pwdHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Password hash error: %v", err)
		data["Error"] = "Failed to hash password."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	// Save User
	userID, err := db.CreateUser(username, string(pwdHash))
	if err != nil {
		log.Printf("User creation error: %v", err)
		data["Error"] = "Failed to register user."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	// Auto-login after registration
	token := generateSessionToken()
	expires := time.Now().Add(24 * time.Hour)
	_ = db.CreateSession(token, userID, expires)

	c.SetCookie(http.Cookie{
		Name:     "session_token",
		Value:    token,
		Expires:  expires,
		HttpOnly: true,
		Path:     "/",
	})

	c.Redirect("/dashboard", http.StatusSeeOther)
	return nil
}

// Handler: POST /logout
func handleLogoutPost(c *nanoserve.Context) error {
	cookie, err := c.GetCookie("session_token")
	if err == nil {
		_ = db.DeleteSession(cookie.Value)
		// Expire cookie
		c.SetCookie(http.Cookie{
			Name:     "session_token",
			Value:    "",
			Expires:  time.Unix(0, 0),
			HttpOnly: true,
			Path:     "/",
		})
	}
	c.Redirect("/", http.StatusSeeOther)
	return nil
}

// Handler: GET /dashboard
func handleDashboardGet(c *nanoserve.Context) error {
	user := getLoggedInUser(c.Request)
	if user == nil {
		c.Redirect("/login", http.StatusSeeOther)
		return nil
	}

	// Get list of active resumes for this user
	resumes, err := db.GetResumesByUserID(user.ID)
	if err != nil {
		log.Printf("Error fetching dashboard resumes: %v", err)
		return err
	}

	data := map[string]interface{}{
		"Username": user.Username,
		"Resumes":  resumes,
	}

	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(c.Writer, "dashboard.html", data)
}

// Handler: POST /upload (Creates a new resume link, user must be logged in)
func handleUpload(c *nanoserve.Context) error {
	user := getLoggedInUser(c.Request)
	if user == nil {
		writeJSONError(c.Writer, http.StatusUnauthorized, "Unauthorized. Please log in.")
		return nil
	}

	// Limit body to 11MB
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 11*1024*1024)
	err := c.Request.ParseMultipartForm(11 * 1024 * 1024)
	if err != nil {
		writeJSONError(c.Writer, http.StatusBadRequest, "File size limit exceeded (Max 10MB) or invalid form data.")
		return nil
	}

	slug := strings.TrimSpace(c.Request.FormValue("slug"))
	if slug == "" {
		writeJSONError(c.Writer, http.StatusBadRequest, "Slug is required.")
		return nil
	}

	if len(slug) < 3 || len(slug) > 30 {
		writeJSONError(c.Writer, http.StatusBadRequest, "Slug must be between 3 and 30 characters.")
		return nil
	}

	// Check if slug taken
	existing, err := db.GetResume(slug)
	if err != nil {
		log.Printf("Slug check error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Database error.")
		return nil
	}
	if existing != nil {
		writeJSONError(c.Writer, http.StatusConflict, "This custom slug is already taken.")
		return nil
	}

	// File check
	file, header, err := c.Request.FormFile("resume")
	if err != nil {
		writeJSONError(c.Writer, http.StatusBadRequest, "No resume PDF uploaded.")
		return nil
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".pdf") {
		writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Unsupported file format. Only PDF files allowed.")
		return nil
	}

	if header.Header.Get("Content-Type") != "application/pdf" {
		writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Invalid file content type. Must be application/pdf.")
		return nil
	}

	if header.Size > 10*1024*1024 {
		writeJSONError(c.Writer, http.StatusBadRequest, "File size exceeds 10MB.")
		return nil
	}

	buf := make([]byte, 512)
	_, _ = file.Read(buf)
	_, _ = file.Seek(0, io.SeekStart)
	if http.DetectContentType(buf) != "application/pdf" {
		writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Security check failed. Not a valid PDF file.")
		return nil
	}

	if r2Client == nil {
		writeJSONError(c.Writer, http.StatusInternalServerError, "R2 client not configured.")
		return nil
	}

	// Upload to R2
	r2Key := "resumes/" + slug + ".pdf"
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	err = r2Client.UploadFile(ctx, r2Key, file, "application/pdf")
	if err != nil {
		log.Printf("R2 upload error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to upload to storage.")
		return nil
	}

	// Save to DB (associated with userID)
	err = db.CreateResume(user.ID, slug, r2Key, filepath.Base(header.Filename))
	if err != nil {
		log.Printf("DB save error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to register resume metadata.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{
		"slug":     slug,
		"filename": header.Filename,
	})
}

// Handler: POST /r/:slug/update (Updates an existing resume, user must own it)
func handleUpdateResume(c *nanoserve.Context) error {
	user := getLoggedInUser(c.Request)
	if user == nil {
		writeJSONError(c.Writer, http.StatusUnauthorized, "Unauthorized.")
		return nil
	}

	slug := c.Param("slug")
	if slug == "" {
		writeJSONError(c.Writer, http.StatusBadRequest, "Slug is required.")
		return nil
	}

	resume, err := db.GetResume(slug)
	if err != nil {
		log.Printf("DB query update error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Database error.")
		return nil
	}

	if resume == nil {
		writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}

	// Verify Ownership
	if resume.UserID != user.ID {
		writeJSONError(c.Writer, http.StatusForbidden, "Forbidden. You do not own this resume.")
		return nil
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 11*1024*1024)
	err = c.Request.ParseMultipartForm(11 * 1024 * 1024)
	if err != nil {
		writeJSONError(c.Writer, http.StatusBadRequest, "File exceeds 10MB limit.")
		return nil
	}

	file, header, err := c.Request.FormFile("resume")
	if err != nil {
		writeJSONError(c.Writer, http.StatusBadRequest, "No PDF file uploaded.")
		return nil
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".pdf") || header.Header.Get("Content-Type") != "application/pdf" {
		writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Only PDF files are supported.")
		return nil
	}

	buf := make([]byte, 512)
	_, _ = file.Read(buf)
	_, _ = file.Seek(0, io.SeekStart)
	if http.DetectContentType(buf) != "application/pdf" {
		writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Not a valid PDF file.")
		return nil
	}

	if r2Client == nil {
		writeJSONError(c.Writer, http.StatusInternalServerError, "R2 client not configured.")
		return nil
	}

	// Overwrite upload to R2
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	err = r2Client.UploadFile(ctx, resume.R2Key, file, "application/pdf")
	if err != nil {
		log.Printf("R2 upload error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to upload file.")
		return nil
	}

	// Update metadata in DB
	err = db.UpdateResume(slug, resume.R2Key, filepath.Base(header.Filename))
	if err != nil {
		log.Printf("DB update error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to save updates.")
		return nil
	}

	updated, _ := db.GetResume(slug)

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]interface{}{
		"slug":       slug,
		"filename":   updated.OriginalFilename,
		"updated_at": updated.UpdatedAt.Format(time.RFC3339),
	})
}

// Handler: POST /r/:slug/delete (Deletes a resume from DB and R2, user must own it)
func handleDeleteResume(c *nanoserve.Context) error {
	user := getLoggedInUser(c.Request)
	if user == nil {
		writeJSONError(c.Writer, http.StatusUnauthorized, "Unauthorized.")
		return nil
	}

	slug := c.Param("slug")
	if slug == "" {
		writeJSONError(c.Writer, http.StatusBadRequest, "Slug is required.")
		return nil
	}

	resume, err := db.GetResume(slug)
	if err != nil {
		log.Printf("DB delete query error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Database error.")
		return nil
	}

	if resume == nil {
		writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}

	// Verify Ownership
	if resume.UserID != user.ID {
		writeJSONError(c.Writer, http.StatusForbidden, "Forbidden. You do not own this resume.")
		return nil
	}

	// Delete from Cloudflare R2
	if r2Client != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		
		err = r2Client.DeleteFile(ctx, resume.R2Key)
		if err != nil {
			log.Printf("Warning: Failed to delete R2 object key %s: %v", resume.R2Key, err)
		}
	}

	// Delete from DB
	err = db.DeleteResume(slug)
	if err != nil {
		log.Printf("DB delete execution error: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to delete database record.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{"message": "Resume deleted successfully."})
}

// Handler: GET /r/:slug (Public viewing of the PDF)
func handleViewResume(c *nanoserve.Context) error {
	slug := c.Param("slug")
	if slug == "" || !isValidSlug(slug) {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	resume, err := db.GetResume(slug)
	if err != nil {
		log.Printf("DB error fetching resume: %v", err)
		http.Error(c.Writer, "Database error", http.StatusInternalServerError)
		return nil
	}

	if resume == nil {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	// Apply security headers on view page
	c.SetHeader("X-Content-Type-Options", "nosniff")
	c.SetHeader("X-Frame-Options", "DENY")
	c.SetHeader("Content-Security-Policy", "default-src 'self'; frame-src 'self'; frame-ancestors 'none'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com;")
	c.SetHeader("Referrer-Policy", "strict-origin-when-cross-origin")
	c.SetHeader("Content-Type", "text/html; charset=utf-8")

	return tmpl.ExecuteTemplate(c.Writer, "view.html", resume)
}

// Handler: GET /r/:slug/raw (Public streaming of the PDF)
func handleStreamResume(c *nanoserve.Context) error {
	slug := c.Param("slug")
	if slug == "" || !isValidSlug(slug) {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	resume, err := db.GetResume(slug)
	if err != nil {
		log.Printf("DB error: %v", err)
		http.Error(c.Writer, "Database error", http.StatusInternalServerError)
		return nil
	}

	if resume == nil {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	if r2Client == nil {
		http.Error(c.Writer, "R2 Client not initialized", http.StatusInternalServerError)
		return nil
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// Download from R2
	body, err := r2Client.DownloadFile(ctx, resume.R2Key)
	if err != nil {
		log.Printf("R2 download error for key %s: %v", resume.R2Key, err)
		http.Error(c.Writer, "Failed to retrieve resume from storage", http.StatusInternalServerError)
		return nil
	}
	defer body.Close()

	// Set PDF headers and security headers for inline browser display
	c.SetHeader("X-Content-Type-Options", "nosniff")
	c.SetHeader("X-Frame-Options", "SAMEORIGIN") // Only allow our own site to frame the raw PDF
	c.SetHeader("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self';")
	c.SetHeader("Referrer-Policy", "no-referrer")
	c.SetHeader("Content-Type", "application/pdf")
	c.SetHeader("Content-Disposition", "inline; filename=\""+resume.OriginalFilename+"\"")

	// Stream the bytes to response writer
	_, err = io.Copy(c.Writer, body)
	if err != nil {
		log.Printf("Error streaming R2 object: %v", err)
	}
	return nil
}

// Handler: POST /delete-account (Permanently deletes the logged-in user and all their R2 + DB files)
func handleDeleteAccount(c *nanoserve.Context) error {
	user := getLoggedInUser(c.Request)
	if user == nil {
		writeJSONError(c.Writer, http.StatusUnauthorized, "Unauthorized. Please log in.")
		return nil
	}

	// 1. Get all resumes belonging to this user
	resumes, err := db.GetResumesByUserID(user.ID)
	if err != nil {
		log.Printf("Error fetching user resumes for deletion: %v", err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Database lookup failed.")
		return nil
	}

	// 2. Delete each file from Cloudflare R2
	if r2Client != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		defer cancel()
		for _, r := range resumes {
			err = r2Client.DeleteFile(ctx, r.R2Key)
			if err != nil {
				log.Printf("Warning: Failed to delete R2 key %s during account deletion: %v", r.R2Key, err)
				// We still proceed so the account can be deleted
			}
		}
	}

	// 3. Atomically delete resumes, sessions, and user from database
	err = db.DeleteUserAndResources(user.ID)
	if err != nil {
		log.Printf("Error deleting user tables for ID %d: %v", user.ID, err)
		writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to delete account tables.")
		return nil
	}

	// 4. Clear cookie
	c.SetCookie(http.Cookie{
		Name:     "session_token",
		Value:    "",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Path:     "/",
	})

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{
		"message": "Account and all associated resources deleted successfully.",
	})
}
