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

	log.Printf("Starting Apsthira with Login Authentication...")
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

	// 5. Setup Router
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleIndex)
	
	// Auth Routes
	mux.HandleFunc("GET /login", handleLoginGet)
	mux.HandleFunc("POST /login", handleLoginPost)
	mux.HandleFunc("GET /register", handleRegisterGet)
	mux.HandleFunc("POST /register", handleRegisterPost)
	mux.HandleFunc("POST /logout", handleLogoutPost)
	
	// Dashboard & Admin Routes
	mux.HandleFunc("GET /dashboard", handleDashboardGet)
	mux.HandleFunc("POST /upload", handleUpload)
	mux.HandleFunc("POST /r/{slug}/update", handleUpdateResume)
	mux.HandleFunc("POST /r/{slug}/delete", handleDeleteResume)
	
	// Public Routes
	mux.HandleFunc("GET /r/{slug}", handleViewResume)
	mux.HandleFunc("GET /r/{slug}/raw", handleStreamResume)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	log.Printf("Server listening on http://localhost:%s", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	
	user := getLoggedInUser(r)
	if user != nil {
		// Already logged in, redirect to dashboard
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		http.Error(w, "Template execution failed", http.StatusInternalServerError)
	}
}

// Handler: GET /login
func handleLoginGet(w http.ResponseWriter, r *http.Request) {
	user := getLoggedInUser(r)
	if user != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "login.html", nil)
}

// Handler: POST /login
func handleLoginPost(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	data := map[string]interface{}{}

	if username == "" || password == "" {
		data["Error"] = "Username and password are required."
		w.WriteHeader(http.StatusBadRequest)
		_ = tmpl.ExecuteTemplate(w, "login.html", data)
		return
	}

	user, err := db.GetUserByUsername(username)
	if err != nil {
		log.Printf("Login lookup error: %v", err)
		data["Error"] = "Internal database error."
		w.WriteHeader(http.StatusInternalServerError)
		_ = tmpl.ExecuteTemplate(w, "login.html", data)
		return
	}

	if user == nil {
		data["Error"] = "Invalid username or password."
		w.WriteHeader(http.StatusUnauthorized)
		_ = tmpl.ExecuteTemplate(w, "login.html", data)
		return
	}

	// Compare password hash
	err = bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password))
	if err != nil {
		data["Error"] = "Invalid username or password."
		w.WriteHeader(http.StatusUnauthorized)
		_ = tmpl.ExecuteTemplate(w, "login.html", data)
		return
	}

	// Create session
	token := generateSessionToken()
	expires := time.Now().Add(24 * time.Hour) // 1 day session
	err = db.CreateSession(token, user.ID, expires)
	if err != nil {
		log.Printf("Session creation error: %v", err)
		data["Error"] = "Failed to initiate session."
		w.WriteHeader(http.StatusInternalServerError)
		_ = tmpl.ExecuteTemplate(w, "login.html", data)
		return
	}

	// Set cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Expires:  expires,
		HttpOnly: true,
		Path:     "/",
	})

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// Handler: GET /register
func handleRegisterGet(w http.ResponseWriter, r *http.Request) {
	user := getLoggedInUser(r)
	if user != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "register.html", nil)
}

// Handler: POST /register
func handleRegisterPost(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	data := map[string]interface{}{}

	if username == "" || password == "" {
		data["Error"] = "Username and password are required."
		w.WriteHeader(http.StatusBadRequest)
		_ = tmpl.ExecuteTemplate(w, "register.html", data)
		return
	}

	if len(username) < 3 || len(password) < 6 {
		data["Error"] = "Username must be at least 3 chars and password at least 6 chars."
		w.WriteHeader(http.StatusBadRequest)
		_ = tmpl.ExecuteTemplate(w, "register.html", data)
		return
	}

	// Check if username taken
	existing, err := db.GetUserByUsername(username)
	if err != nil {
		log.Printf("Registration username check error: %v", err)
		data["Error"] = "Database lookup failed."
		w.WriteHeader(http.StatusInternalServerError)
		_ = tmpl.ExecuteTemplate(w, "register.html", data)
		return
	}

	if existing != nil {
		data["Error"] = "Username is already taken."
		w.WriteHeader(http.StatusConflict)
		_ = tmpl.ExecuteTemplate(w, "register.html", data)
		return
	}

	// Hash password
	pwdHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Password hash error: %v", err)
		data["Error"] = "Failed to hash password."
		w.WriteHeader(http.StatusInternalServerError)
		_ = tmpl.ExecuteTemplate(w, "register.html", data)
		return
	}

	// Save User
	userID, err := db.CreateUser(username, string(pwdHash))
	if err != nil {
		log.Printf("User creation error: %v", err)
		data["Error"] = "Failed to register user."
		w.WriteHeader(http.StatusInternalServerError)
		_ = tmpl.ExecuteTemplate(w, "register.html", data)
		return
	}

	// Auto-login after registration
	token := generateSessionToken()
	expires := time.Now().Add(24 * time.Hour)
	_ = db.CreateSession(token, userID, expires)

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Expires:  expires,
		HttpOnly: true,
		Path:     "/",
	})

	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// Handler: POST /logout
func handleLogoutPost(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil {
		_ = db.DeleteSession(cookie.Value)
		// Expire cookie
		http.SetCookie(w, &http.Cookie{
			Name:     "session_token",
			Value:    "",
			Expires:  time.Unix(0, 0),
			HttpOnly: true,
			Path:     "/",
		})
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Handler: GET /dashboard
func handleDashboardGet(w http.ResponseWriter, r *http.Request) {
	user := getLoggedInUser(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Get list of active resumes for this user
	resumes, err := db.GetResumesByUserID(user.ID)
	if err != nil {
		log.Printf("Error fetching dashboard resumes: %v", err)
		http.Error(w, "Failed to load dashboard data.", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Username": user.Username,
		"Resumes":  resumes,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "dashboard.html", data)
}

// Handler: POST /upload (Creates a new resume link, user must be logged in)
func handleUpload(w http.ResponseWriter, r *http.Request) {
	user := getLoggedInUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "Unauthorized. Please log in.")
		return
	}

	// Limit body to 11MB
	r.Body = http.MaxBytesReader(w, r.Body, 11*1024*1024)
	err := r.ParseMultipartForm(11 * 1024 * 1024)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "File size limit exceeded (Max 10MB) or invalid form data.")
		return
	}

	slug := strings.TrimSpace(r.FormValue("slug"))
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "Slug is required.")
		return
	}

	if len(slug) < 3 || len(slug) > 30 {
		writeJSONError(w, http.StatusBadRequest, "Slug must be between 3 and 30 characters.")
		return
	}

	// Check if slug taken
	existing, err := db.GetResume(slug)
	if err != nil {
		log.Printf("Slug check error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error.")
		return
	}
	if existing != nil {
		writeJSONError(w, http.StatusConflict, "This custom slug is already taken.")
		return
	}

	// File check
	file, header, err := r.FormFile("resume")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "No resume PDF uploaded.")
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".pdf") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "Unsupported file format. Only PDF files allowed.")
		return
	}

	if header.Header.Get("Content-Type") != "application/pdf" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "Invalid file content type. Must be application/pdf.")
		return
	}

	if header.Size > 10*1024*1024 {
		writeJSONError(w, http.StatusBadRequest, "File size exceeds 10MB.")
		return
	}

	buf := make([]byte, 512)
	_, _ = file.Read(buf)
	_, _ = file.Seek(0, io.SeekStart)
	if http.DetectContentType(buf) != "application/pdf" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "Security check failed. Not a valid PDF file.")
		return
	}

	if r2Client == nil {
		writeJSONError(w, http.StatusInternalServerError, "R2 client not configured.")
		return
	}

	// Upload to R2
	r2Key := "resumes/" + slug + ".pdf"
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	err = r2Client.UploadFile(ctx, r2Key, file, "application/pdf")
	if err != nil {
		log.Printf("R2 upload error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to upload to storage.")
		return
	}

	// Save to DB (associated with userID)
	err = db.CreateResume(user.ID, slug, r2Key, filepath.Base(header.Filename))
	if err != nil {
		log.Printf("DB save error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to register resume metadata.")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"slug":     slug,
		"filename": header.Filename,
	})
}

// Handler: POST /r/{slug}/update (Updates an existing resume, user must own it)
func handleUpdateResume(w http.ResponseWriter, r *http.Request) {
	user := getLoggedInUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "Unauthorized.")
		return
	}

	slug := r.PathValue("slug")
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "Slug is required.")
		return
	}

	resume, err := db.GetResume(slug)
	if err != nil {
		log.Printf("DB query update error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error.")
		return
	}

	if resume == nil {
		writeJSONError(w, http.StatusNotFound, "Resume not found.")
		return
	}

	// Verify Ownership
	if resume.UserID != user.ID {
		writeJSONError(w, http.StatusForbidden, "Forbidden. You do not own this resume.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 11*1024*1024)
	err = r.ParseMultipartForm(11 * 1024 * 1024)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "File exceeds 10MB limit.")
		return
	}

	file, header, err := r.FormFile("resume")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "No PDF file uploaded.")
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".pdf") || header.Header.Get("Content-Type") != "application/pdf" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "Only PDF files are supported.")
		return
	}

	buf := make([]byte, 512)
	_, _ = file.Read(buf)
	_, _ = file.Seek(0, io.SeekStart)
	if http.DetectContentType(buf) != "application/pdf" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "Not a valid PDF file.")
		return
	}

	if r2Client == nil {
		writeJSONError(w, http.StatusInternalServerError, "R2 client not configured.")
		return
	}

	// Overwrite upload to R2
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	err = r2Client.UploadFile(ctx, resume.R2Key, file, "application/pdf")
	if err != nil {
		log.Printf("R2 upload error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to upload file.")
		return
	}

	// Update metadata in DB
	err = db.UpdateResume(slug, resume.R2Key, filepath.Base(header.Filename))
	if err != nil {
		log.Printf("DB update error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to save updates.")
		return
	}

	updated, _ := db.GetResume(slug)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"slug":       slug,
		"filename":   updated.OriginalFilename,
		"updated_at": updated.UpdatedAt.Format(time.RFC3339),
	})
}

// Handler: POST /r/{slug}/delete (Deletes a resume from DB and R2, user must own it)
func handleDeleteResume(w http.ResponseWriter, r *http.Request) {
	user := getLoggedInUser(r)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "Unauthorized.")
		return
	}

	slug := r.PathValue("slug")
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "Slug is required.")
		return
	}

	resume, err := db.GetResume(slug)
	if err != nil {
		log.Printf("DB delete query error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Database error.")
		return
	}

	if resume == nil {
		writeJSONError(w, http.StatusNotFound, "Resume not found.")
		return
	}

	// Verify Ownership
	if resume.UserID != user.ID {
		writeJSONError(w, http.StatusForbidden, "Forbidden. You do not own this resume.")
		return
	}

	// Delete from Cloudflare R2
	if r2Client != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		
		err = r2Client.DeleteFile(ctx, resume.R2Key)
		if err != nil {
			log.Printf("Warning: Failed to delete R2 object key %s: %v", resume.R2Key, err)
			// We'll proceed to delete from SQLite anyway so the slug is freed up
		}
	}

	// Delete from DB
	err = db.DeleteResume(slug)
	if err != nil {
		log.Printf("DB delete execution error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to delete database record.")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "Resume deleted successfully."})
}

// Handler: GET /r/{slug} (Public viewing of the PDF)
func handleViewResume(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" || !isValidSlug(slug) {
		http.NotFound(w, r)
		return
	}

	resume, err := db.GetResume(slug)
	if err != nil {
		log.Printf("DB error fetching resume: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if resume == nil {
		http.NotFound(w, r)
		return
	}

	// Apply security headers on view page
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-src 'self'; frame-ancestors 'none'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com;")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := tmpl.ExecuteTemplate(w, "view.html", resume); err != nil {
		log.Printf("Template render error: %v", err)
		http.Error(w, "Template execution failed", http.StatusInternalServerError)
	}
}

// Handler: GET /r/{slug}/raw (Public streaming of the PDF)
func handleStreamResume(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" || !isValidSlug(slug) {
		http.NotFound(w, r)
		return
	}

	resume, err := db.GetResume(slug)
	if err != nil {
		log.Printf("DB error: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if resume == nil {
		http.NotFound(w, r)
		return
	}

	if r2Client == nil {
		http.Error(w, "R2 Client not initialized", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Download from R2
	body, err := r2Client.DownloadFile(ctx, resume.R2Key)
	if err != nil {
		log.Printf("R2 download error for key %s: %v", resume.R2Key, err)
		http.Error(w, "Failed to retrieve resume from storage", http.StatusInternalServerError)
		return
	}
	defer body.Close()

	// Set PDF headers and security headers for inline browser display
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN") // Only allow our own site to frame the raw PDF
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self';")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline; filename=\""+resume.OriginalFilename+"\"")

	// Stream the bytes to response writer
	_, err = io.Copy(w, body)
	if err != nil {
		log.Printf("Error streaming R2 object: %v", err)
	}
}
