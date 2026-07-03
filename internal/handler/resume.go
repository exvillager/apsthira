package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/exvillager/nanoserve"
)

var slugRegex = regexp.MustCompile(`^[a-zA-Z0-9-_]{3,30}$`)

func isValidSlug(slug string) bool {
	return slugRegex.MatchString(slug)
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
	if len(resumes) >= 3 {
		h.writeJSONError(c.Writer, http.StatusForbidden, "Resume upload limit reached. You can only create up to 3 links.")
		return nil
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 11*1024*1024)
	if err := c.Request.ParseMultipartForm(11 * 1024 * 1024); err != nil {
		h.writeJSONError(c.Writer, http.StatusBadRequest, "File size limit exceeded (Max 10MB) or invalid form data.")
		return nil
	}

	slug := strings.ToLower(strings.TrimSpace(c.Request.FormValue("slug")))
	if slug == "" {
		// auto-generate a unique random slug
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

	r2Key := "resumes/" + slug + ".pdf"
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	if err = h.R2.UploadFile(ctx, r2Key, file, "application/pdf"); err != nil {
		slog.Error("R2 upload error", "key", r2Key, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to upload to storage.")
		return nil
	}

	if err = h.DB.CreateResume(user.ID, slug, r2Key, filepath.Base(header.Filename)); err != nil {
		slog.Error("DB save error", "slug", slug, "error", err)
		
		// Clean up the orphaned file in R2 asynchronously
		go func(key string) {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cleanupCancel()
			if cleanupErr := h.R2.DeleteFile(cleanupCtx, key); cleanupErr != nil {
				slog.Error("failed to clean up orphaned R2 file", "key", key, "error", cleanupErr)
			} else {
				slog.Info("successfully cleaned up orphaned R2 file", "key", key)
			}
		}(r2Key)

		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to register resume metadata.")
		return nil
	}

	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{
		"slug":     slug,
		"filename": header.Filename,
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

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	if err = h.R2.UploadFile(ctx, resume.R2Key, file, "application/pdf"); err != nil {
		slog.Error("R2 upload error", "key", resume.R2Key, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to upload file.")
		return nil
	}

	filename := filepath.Base(header.Filename)
	if err = h.DB.UpdateResume(slug, resume.R2Key, filename); err != nil {
		slog.Error("DB update error", "slug", slug, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to save updates.")
		return nil
	}

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

	if h.R2 != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		if err = h.R2.DeleteFile(ctx, resume.R2Key); err != nil {
			slog.Warn("failed to delete R2 object", "key", resume.R2Key, "error", err)
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

	c.SetHeader("X-Content-Type-Options", "nosniff")
	c.SetHeader("X-Frame-Options", "DENY")
	c.SetHeader("Content-Security-Policy", "default-src 'self'; frame-src 'self'; frame-ancestors 'none'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com;")
	c.SetHeader("Referrer-Policy", "strict-origin-when-cross-origin")
	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return h.Tmpl.ExecuteTemplate(c.Writer, "view.html", resume)
}

func (h *Handler) HandleStreamResume(c *nanoserve.Context) error {
	slug := strings.ToLower(c.Param("slug"))
	if slug == "" || !isValidSlug(slug) {
		http.NotFound(c.Writer, c.Request)
		return nil
	}

	resume, err := h.DB.GetResume(slug)
	if err != nil {
		slog.Error("DB error fetching resume for stream", "slug", slug, "error", err)
		http.Error(c.Writer, "Database error", http.StatusInternalServerError)
		return nil
	}
	if resume == nil {
		http.NotFound(c.Writer, c.Request)
		return nil
	}
	if h.R2 == nil {
		http.Error(c.Writer, "R2 Client not initialized", http.StatusInternalServerError)
		return nil
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	body, err := h.R2.DownloadFile(ctx, resume.R2Key)
	if err != nil {
		slog.Error("R2 download error", "key", resume.R2Key, "error", err)
		http.Error(c.Writer, "Failed to retrieve resume from storage", http.StatusInternalServerError)
		return nil
	}
	defer body.Close()

	c.SetHeader("X-Content-Type-Options", "nosniff")
	c.SetHeader("X-Frame-Options", "SAMEORIGIN")
	c.SetHeader("Content-Security-Policy", "default-src 'none'; frame-ancestors 'self';")
	c.SetHeader("Referrer-Policy", "no-referrer")
	c.SetHeader("Content-Type", "application/pdf")
	c.SetHeader("Content-Disposition", "inline; filename=\""+resume.OriginalFilename+"\"")

	if _, err = io.Copy(c.Writer, body); err != nil {
		slog.Error("error streaming R2 object", "key", resume.R2Key, "error", err)
	}
	return nil
}
