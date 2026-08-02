package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/exvillager/nanoserve"
	"github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

var slugRegex = regexp.MustCompile(`^[a-zA-Z0-9-_]{3,30}$`)

func isValidSlug(slug string) bool {
	return slugRegex.MatchString(slug)
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}

func hashIP(ip string) string {
	h := sha256.Sum256([]byte(ip + "_apsthira_salt"))
	return hex.EncodeToString(h[:8])
}

func detectDeviceType(ua string) string {
	uaLower := strings.ToLower(ua)
	if strings.Contains(uaLower, "ipad") || strings.Contains(uaLower, "tablet") {
		return "tablet"
	}
	if strings.Contains(uaLower, "mobile") || strings.Contains(uaLower, "android") || strings.Contains(uaLower, "iphone") {
		return "mobile"
	}
	return "desktop"
}

func getPublicURL(r *http.Request, slug string) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/r/%s", scheme, r.Host, slug)
}

func (h *Handler) HandleDashboardGet(c *nanoserve.Context) error {
	user := h.mustGetUser(c)

	resumes, err := h.DB.GetResumesByUserID(user.ID)
	if err != nil {
		slog.Error("error fetching dashboard resumes", "error", err)
		return err
	}

	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return h.Tmpl.ExecuteTemplate(c.Writer, "dashboard.html", map[string]any{
		"Username": user.Username,
		"Resumes":  resumes,
		"Host":     c.Request.Host,
	})
}

func (h *Handler) HandleUpload(c *nanoserve.Context) error {
	user := h.mustGetUser(c)

	resumes, err := h.DB.GetResumesByUserID(user.ID)
	if err != nil {
		slog.Error("error checking user resume count limit", "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Database error.")
		return nil
	}
	if len(resumes) >= 5 {
		h.writeJSONError(c.Writer, http.StatusForbidden, "Resume upload limit reached. You can create up to 5 links.")
		return nil
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 11*1024*1024)
	if err := c.Request.ParseMultipartForm(11 * 1024 * 1024); err != nil {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "File size limit exceeded (Max 10MB) or invalid form data.")
		return nil
	}

	slug := strings.ToLower(strings.TrimSpace(c.Request.FormValue("slug")))
	if slug == "" {
		for range 5 {
			candidate := generateSlug()
			existing, err := h.DB.GetResume(candidate)
			if err != nil {
				slog.Error("slug check error", "slug", candidate, "error", err)
				h.writeJSONError(c.Writer, http.StatusInternalServerError, "Database error.")
				return nil
			}
			if existing == nil {
				slug = candidate
				break
			}
		}
		if slug == "" {
			h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to generate a unique slug. Please try again.")
			return nil
		}
	} else {
		if len(slug) < 3 || len(slug) > 30 {
			h.writeJSONError(c.Writer, http.StatusBadRequest, "Slug must be between 3 and 30 characters.")
			return nil
		}
		if !isValidSlug(slug) {
			h.writeJSONError(c.Writer, http.StatusBadRequest, "Slug can only contain alphanumeric characters, hyphens, and underscores.")
			return nil
		}
		existing, err := h.DB.GetResume(slug)
		if err != nil {
			slog.Error("slug check error", "slug", slug, "error", err)
			h.writeJSONError(c.Writer, http.StatusInternalServerError, "Database error.")
			return nil
		}
		if existing != nil {
			h.writeJSONError(c.Writer, http.StatusConflict, "This custom slug is already taken.")
			return nil
		}
	}

	file, header, err := c.Request.FormFile("resume")
	if err != nil {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "No resume PDF uploaded.")
		return nil
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".pdf") {
		h.writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Unsupported file format. Only PDF files allowed.")
		return nil
	}
	if header.Header.Get("Content-Type") != "application/pdf" {
		h.writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Invalid file content type. Must be application/pdf.")
		return nil
	}
	if header.Size > 10*1024*1024 {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "File size exceeds 10MB.")
		return nil
	}

	buf := make([]byte, 512)
	_, _ = file.Read(buf)
	_, _ = file.Seek(0, io.SeekStart)
	if http.DetectContentType(buf) != "application/pdf" {
		h.writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Security check failed. Not a valid PDF file.")
		return nil
	}

	if h.R2 == nil {
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "R2 client not configured.")
		return nil
	}

	r2Key := fmt.Sprintf("resumes/%s_v%d.pdf", slug, time.Now().Unix())
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	if err = h.R2.UploadFile(ctx, r2Key, file, "application/pdf"); err != nil {
		slog.Error("R2 upload error", "key", r2Key, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to upload to storage.")
		return nil
	}

	origFilename := filepath.Base(header.Filename)
	if err = h.DB.CreateResume(user.ID, slug, r2Key, origFilename); err != nil {
		slog.Error("DB save error", "slug", slug, "error", err)
		go func(key string) {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cleanupCancel()
			_ = h.R2.DeleteFile(cleanupCtx, key)
		}(r2Key)

		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to register resume metadata.")
		return nil
	}

	// Fetch created resume to get ID & store initial version 1
	createdResume, _ := h.DB.GetResume(slug)
	if createdResume != nil {
		_ = h.DB.AddResumeVersion(createdResume.ID, r2Key, origFilename)
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{
		"slug":     slug,
		"filename": origFilename,
	})
}

func (h *Handler) HandleUpdateResume(c *nanoserve.Context) error {
	user := h.mustGetUser(c)

	slug := strings.ToLower(c.Param("slug"))
	if slug == "" {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "Slug is required.")
		return nil
	}

	resume, err := h.DB.GetResume(slug)
	if err != nil {
		slog.Error("DB query update error", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Database error.")
		return nil
	}
	if resume == nil {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}
	if resume.UserID != user.ID {
		h.writeJSONError(c.Writer, http.StatusForbidden, "Forbidden. You do not own this resume.")
		return nil
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 11*1024*1024)
	if err = c.Request.ParseMultipartForm(11 * 1024 * 1024); err != nil {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "File exceeds 10MB limit.")
		return nil
	}

	file, header, err := c.Request.FormFile("resume")
	if err != nil {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "No PDF file uploaded.")
		return nil
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".pdf") || header.Header.Get("Content-Type") != "application/pdf" {
		h.writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Only PDF files are supported.")
		return nil
	}

	buf := make([]byte, 512)
	_, _ = file.Read(buf)
	_, _ = file.Seek(0, io.SeekStart)
	if http.DetectContentType(buf) != "application/pdf" {
		h.writeJSONError(c.Writer, http.StatusUnsupportedMediaType, "Not a valid PDF file.")
		return nil
	}

	if h.R2 == nil {
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "R2 client not configured.")
		return nil
	}

	newR2Key := fmt.Sprintf("resumes/%s_v%d.pdf", slug, time.Now().Unix())
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	if err = h.R2.UploadFile(ctx, newR2Key, file, "application/pdf"); err != nil {
		slog.Error("R2 upload error", "key", newR2Key, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to upload file.")
		return nil
	}

	// Store previous version in version history table before updating
	_ = h.DB.AddResumeVersion(resume.ID, resume.R2Key, resume.OriginalFilename)

	filename := filepath.Base(header.Filename)
	if err = h.DB.UpdateResume(slug, newR2Key, filename); err != nil {
		slog.Error("DB update error", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to save updates.")
		return nil
	}

	// Record new active file as a new version entry
	_ = h.DB.AddResumeVersion(resume.ID, newR2Key, filename)

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]any{
		"slug":       slug,
		"filename":   filename,
		"updated_at": time.Now().Format(time.RFC3339),
	})
}

func (h *Handler) HandleDeleteResume(c *nanoserve.Context) error {
	user := h.mustGetUser(c)

	slug := strings.ToLower(c.Param("slug"))
	if slug == "" {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "Slug is required.")
		return nil
	}

	resume, err := h.DB.GetResume(slug)
	if err != nil {
		slog.Error("DB delete query error", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Database error.")
		return nil
	}
	if resume == nil {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}
	if resume.UserID != user.ID {
		h.writeJSONError(c.Writer, http.StatusForbidden, "Forbidden. You do not own this resume.")
		return nil
	}

	// Delete active object and historical version objects from R2
	if h.R2 != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		_ = h.R2.DeleteFile(ctx, resume.R2Key)

		if versions, err := h.DB.GetResumeVersions(resume.ID); err == nil {
			for _, v := range versions {
				_ = h.R2.DeleteFile(ctx, v.R2Key)
			}
		}
	}

	if err = h.DB.DeleteResume(slug); err != nil {
		slog.Error("DB delete execution error", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to delete database record.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{"message": "Resume deleted successfully."})
}

func (h *Handler) HandleGetAnalytics(c *nanoserve.Context) error {
	user := h.mustGetUser(c)
	slug := strings.ToLower(c.Param("slug"))

	resume, err := h.DB.GetResume(slug)
	if err != nil || resume == nil {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}
	if resume.UserID != user.ID {
		h.writeJSONError(c.Writer, http.StatusForbidden, "Forbidden.")
		return nil
	}

	summary, err := h.DB.GetResumeAnalytics(resume.ID)
	if err != nil {
		slog.Error("failed to get analytics", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to load analytics.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(summary)
}

func (h *Handler) HandleGetVersions(c *nanoserve.Context) error {
	user := h.mustGetUser(c)
	slug := strings.ToLower(c.Param("slug"))

	resume, err := h.DB.GetResume(slug)
	if err != nil || resume == nil {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}
	if resume.UserID != user.ID {
		h.writeJSONError(c.Writer, http.StatusForbidden, "Forbidden.")
		return nil
	}

	versions, err := h.DB.GetResumeVersions(resume.ID)
	if err != nil {
		slog.Error("failed to get resume versions", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to load version history.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(versions)
}

func (h *Handler) HandleRollbackVersion(c *nanoserve.Context) error {
	user := h.mustGetUser(c)
	slug := strings.ToLower(c.Param("slug"))

	resume, err := h.DB.GetResume(slug)
	if err != nil || resume == nil {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}
	if resume.UserID != user.ID {
		h.writeJSONError(c.Writer, http.StatusForbidden, "Forbidden.")
		return nil
	}

	var req struct {
		VersionID int64 `json:"version_id"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		// try form value fallback
		vStr := c.Request.FormValue("version_id")
		vID, _ := strconv.ParseInt(vStr, 10, 64)
		req.VersionID = vID
	}
	if req.VersionID == 0 {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "Invalid version_id.")
		return nil
	}

	version, err := h.DB.GetResumeVersionByID(req.VersionID)
	if err != nil || version == nil || version.ResumeID != resume.ID {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Specified version not found.")
		return nil
	}

	// Archive current active state if it differs
	if resume.R2Key != version.R2Key {
		_ = h.DB.AddResumeVersion(resume.ID, resume.R2Key, resume.OriginalFilename)
	}

	if err = h.DB.UpdateResume(slug, version.R2Key, version.OriginalFilename); err != nil {
		slog.Error("rollback DB update error", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to rollback version.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]any{
		"message":           "Resume rolled back successfully.",
		"version_num":       version.VersionNum,
		"original_filename": version.OriginalFilename,
	})
}

func (h *Handler) HandleUpdateSettings(c *nanoserve.Context) error {
	user := h.mustGetUser(c)
	slug := strings.ToLower(c.Param("slug"))

	resume, err := h.DB.GetResume(slug)
	if err != nil || resume == nil {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}
	if resume.UserID != user.ID {
		h.writeJSONError(c.Writer, http.StatusForbidden, "Forbidden.")
		return nil
	}

	var req struct {
		Passcode      string `json:"passcode"`
		RemovePasscode bool   `json:"remove_passcode"`
		ExpiresAt     string `json:"expires_at"`
		ClearExpiry   bool   `json:"clear_expiry"`
		AllowDownload bool   `json:"allow_download"`
	}

	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		req.Passcode = c.Request.FormValue("passcode")
		req.RemovePasscode = c.Request.FormValue("remove_passcode") == "true"
		req.ExpiresAt = c.Request.FormValue("expires_at")
		req.ClearExpiry = c.Request.FormValue("clear_expiry") == "true"
		req.AllowDownload = c.Request.FormValue("allow_download") == "true" || c.Request.FormValue("allow_download") == "1"
	}

	passcodeHash := resume.PasscodeHash
	if req.RemovePasscode {
		passcodeHash = ""
	} else if strings.TrimSpace(req.Passcode) != "" {
		hashed, err := bcrypt.GenerateFromPassword([]byte(strings.TrimSpace(req.Passcode)), bcrypt.DefaultCost)
		if err != nil {
			h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to process passcode.")
			return nil
		}
		passcodeHash = string(hashed)
	}

	var expTime *time.Time
	if req.ClearExpiry {
		expTime = nil
	} else if strings.TrimSpace(req.ExpiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			parsed, err = time.Parse("2006-01-02T15:04", req.ExpiresAt)
		}
		if err != nil {
			parsed, err = time.Parse("2006-01-02", req.ExpiresAt)
		}
		if err == nil {
			expTime = &parsed
		} else {
			h.writeJSONError(c.Writer, http.StatusBadRequest, "Invalid expiration date format.")
			return nil
		}
	} else {
		expTime = resume.ExpiresAt
	}

	if err := h.DB.UpdateResumeSettings(slug, passcodeHash, expTime, req.AllowDownload); err != nil {
		slog.Error("failed to update settings", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to update settings.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{"message": "Resume settings updated successfully."})
}

func (h *Handler) HandleUnlockResume(c *nanoserve.Context) error {
	slug := strings.ToLower(c.Param("slug"))
	resume, err := h.DB.GetResume(slug)
	if err != nil || resume == nil {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}

	if resume.PasscodeHash == "" {
		c.SetHeader("Content-Type", "application/json")
		return json.NewEncoder(c.Writer).Encode(map[string]bool{"unlocked": true})
	}

	var req struct {
		Passcode string `json:"passcode"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		req.Passcode = c.Request.FormValue("passcode")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(resume.PasscodeHash), []byte(strings.TrimSpace(req.Passcode))); err != nil {
		h.writeJSONError(c.Writer, http.StatusUnauthorized, "Incorrect passcode.")
		return nil
	}

	// Set unlock cookie
	cookie := http.Cookie{
		Name:     "unlocked_" + slug,
		Value:    "1",
		Path:     "/r/" + slug,
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	http.SetCookie(c.Writer, &cookie)

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]bool{"success": true})
}

func (h *Handler) HandleGetQRCode(c *nanoserve.Context) error {
	slug := strings.ToLower(c.Param("slug"))
	if slug == "" || !isValidSlug(slug) {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	resume, err := h.DB.GetResume(slug)
	if err != nil || resume == nil {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	publicURL := getPublicURL(c.Request, slug)
	pngBytes, err := qrcode.Encode(publicURL, qrcode.Medium, 300)
	if err != nil {
		slog.Error("failed to generate QR code", "slug", slug, "error", err)
		http.Error(c.Writer, "Failed to generate QR code", http.StatusInternalServerError)
		return nil
	}

	c.SetHeader("Content-Type", "image/png")
	c.SetHeader("Cache-Control", "public, max-age=86400")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(pngBytes)
	return nil
}

func (h *Handler) HandleViewResume(c *nanoserve.Context) error {
	slug := strings.ToLower(c.Param("slug"))
	if slug == "" || !isValidSlug(slug) {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	resume, err := h.DB.GetResume(slug)
	if err != nil {
		slog.Error("DB error fetching resume", "slug", slug, "error", err)
		http.Error(c.Writer, "Database error", http.StatusInternalServerError)
		return nil
	}
	if resume == nil {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	// 1. Check Expiration
	isExpired := false
	if resume.ExpiresAt != nil && time.Now().After(*resume.ExpiresAt) {
		isExpired = true
	}

	// 2. Check Passcode Protection
	isProtected := false
	if resume.PasscodeHash != "" {
		user := h.getLoggedInUser(c.Request)
		// Owner bypasses passcode check
		if user == nil || user.ID != resume.UserID {
			cookie, err := c.Request.Cookie("unlocked_" + slug)
			if err != nil || cookie.Value != "1" {
				isProtected = true
			}
		}
	}

	// 3. Log Analytics asynchronously if link is valid and accessed
	if !isExpired && !isProtected {
		referrer := c.Request.Referer()
		ua := c.Request.UserAgent()
		ipHash := hashIP(getClientIP(c.Request))
		devType := detectDeviceType(ua)
		go func(rID int64, ref, userAgent, hash, device string) {
			_ = h.DB.LogResumeView(rID, ref, userAgent, hash, device)
		}(resume.ID, referrer, ua, ipHash, devType)
	}

	c.SetHeader("X-Content-Type-Options", "nosniff")
	c.SetHeader("X-Frame-Options", "DENY")
	c.SetHeader("Content-Security-Policy", "default-src 'self'; frame-src 'self'; frame-ancestors 'none'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; script-src 'self' 'unsafe-inline'; img-src 'self' data:;")
	c.SetHeader("Referrer-Policy", "strict-origin-when-cross-origin")
	c.SetHeader("Content-Type", "text/html; charset=utf-8")

	return h.Tmpl.ExecuteTemplate(c.Writer, "view.html", map[string]any{
		"Slug":             resume.Slug,
		"OriginalFilename": resume.OriginalFilename,
		"ViewsCount":       resume.ViewsCount,
		"PasscodeHash":     resume.PasscodeHash,
		"ExpiresAt":        resume.ExpiresAt,
		"AllowDownload":    resume.AllowDownload,
		"IsExpired":        isExpired,
		"IsProtected":      isProtected,
		"PublicURL":        getPublicURL(c.Request, slug),
		"Host":             c.Request.Host,
	})
}

func (h *Handler) HandleStreamResume(c *nanoserve.Context) error {
	slug := strings.ToLower(c.Param("slug"))
	if slug == "" || !isValidSlug(slug) {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	resume, err := h.DB.GetResume(slug)
	if err != nil || resume == nil {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	// Expiration check
	if resume.ExpiresAt != nil && time.Now().After(*resume.ExpiresAt) {
		http.Error(c.Writer, "Link Expired", http.StatusGone)
		return nil
	}

	// Protection check
	if resume.PasscodeHash != "" {
		user := h.getLoggedInUser(c.Request)
		if user == nil || user.ID != resume.UserID {
			cookie, err := c.Request.Cookie("unlocked_" + slug)
			if err != nil || cookie.Value != "1" {
				http.Error(c.Writer, "Passcode Required", http.StatusUnauthorized)
				return nil
			}
		}
	}

	// Download restriction check
	isDownloadAttempt := c.Request.FormValue("dl") == "1" || c.Request.FormValue("download") == "true"
	if isDownloadAttempt && !resume.AllowDownload {
		http.Error(c.Writer, "Direct PDF downloads are disabled for this resume link.", http.StatusForbidden)
		return nil
	}

	// Custom version requested?
	r2Key := resume.R2Key
	if verIDStr := c.Request.FormValue("v"); verIDStr != "" {
		if verID, parseErr := strconv.ParseInt(verIDStr, 10, 64); parseErr == nil {
			if vObj, vErr := h.DB.GetResumeVersionByID(verID); vErr == nil && vObj != nil && vObj.ResumeID == resume.ID {
				r2Key = vObj.R2Key
			}
		}
	}

	if h.R2 == nil {
		http.Error(c.Writer, "R2 Client not initialized", http.StatusInternalServerError)
		return nil
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	body, err := h.R2.DownloadFile(ctx, r2Key)
	if err != nil {
		slog.Error("R2 download error", "key", r2Key, "error", err)
		http.Error(c.Writer, "Failed to retrieve resume from storage", http.StatusInternalServerError)
		return nil
	}
	defer body.Close()

	disposition := "inline"
	if isDownloadAttempt && resume.AllowDownload {
		disposition = "attachment"
	}

	c.SetHeader("X-Content-Type-Options", "nosniff")
	c.SetHeader("X-Frame-Options", "SAMEORIGIN")
	c.SetHeader("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self';")
	c.SetHeader("Referrer-Policy", "no-referrer")
	c.SetHeader("Content-Type", "application/pdf")
	c.SetHeader("Content-Disposition", fmt.Sprintf("%s; filename=\"%s\"", disposition, resume.OriginalFilename))

	if _, err = io.Copy(c.Writer, body); err != nil {
		slog.Error("error streaming R2 object", "key", r2Key, "error", err)
	}
	return nil
}

func (h *Handler) HandleIncrementViewCount(c *nanoserve.Context) error {
	slug := strings.ToLower(c.Param("slug"))
	if slug == "" || !isValidSlug(slug) {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "Invalid slug.")
		return nil
	}

	resume, err := h.DB.GetResume(slug)
	if err != nil || resume == nil {
		h.writeJSONError(c.Writer, http.StatusNotFound, "Resume not found.")
		return nil
	}

	if err = h.DB.IncrementViews(slug); err != nil {
		slog.Error("failed to increment view count", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to increment view count.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{"message": "View count updated."})
}
