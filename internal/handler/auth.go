package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/exvillager/nanoserve"
	"golang.org/x/crypto/bcrypt"
)

func (h *Handler) HandleIndex(c *nanoserve.Context) error {
	if h.mustGetUser(c) != nil {
		c.Redirect("/dashboard", http.StatusSeeOther)
		return nil
	}
	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return h.Tmpl.ExecuteTemplate(c.Writer, "index.html", nil)
}

func (h *Handler) HandleLoginGet(c *nanoserve.Context) error {
	if h.mustGetUser(c) != nil {
		c.Redirect("/dashboard", http.StatusSeeOther)
		return nil
	}
	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return h.Tmpl.ExecuteTemplate(c.Writer, "login.html", nil)
}

func (h *Handler) HandleLoginPost(c *nanoserve.Context) error {
	if h.mustGetUser(c) != nil {
		c.Redirect("/dashboard", http.StatusSeeOther)
		return nil
	}
	username := strings.ToLower(strings.TrimSpace(c.Request.FormValue("username")))
	password := c.Request.FormValue("password")
	data := map[string]any{}

	if username == "" || password == "" {
		data["Error"] = "Username and password are required."
		c.Status(http.StatusBadRequest)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	user, err := h.DB.GetUserByUsername(username)
	if err != nil {
		slog.Error("login lookup error", "error", err)
		data["Error"] = "Internal database error."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}
	if user == nil {
		data["Error"] = "Invalid username or password."
		c.Status(http.StatusUnauthorized)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	if err = bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		data["Error"] = "Invalid username or password."
		c.Status(http.StatusUnauthorized)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	token := h.generateSessionToken()
	expires := time.Now().Add(30 * time.Minute)
	if err = h.DB.CreateSession(token, user.ID, expires); err != nil {
		slog.Error("session creation error", "error", err)
		data["Error"] = "Failed to initiate session."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "login.html", data)
	}

	c.SetCookie(http.Cookie{Name: "session_token", Value: token, Expires: expires, HttpOnly: true, Path: "/"})
	c.Redirect("/dashboard", http.StatusSeeOther)
	return nil
}

func (h *Handler) HandleRegisterGet(c *nanoserve.Context) error {
	if h.mustGetUser(c) != nil {
		c.Redirect("/dashboard", http.StatusSeeOther)
		return nil
	}
	c.SetHeader("Content-Type", "text/html; charset=utf-8")
	return h.Tmpl.ExecuteTemplate(c.Writer, "register.html", nil)
}

func (h *Handler) HandleRegisterPost(c *nanoserve.Context) error {
	if h.mustGetUser(c) != nil {
		c.Redirect("/dashboard", http.StatusSeeOther)
		return nil
	}
	username := strings.ToLower(strings.TrimSpace(c.Request.FormValue("username")))
	password := c.Request.FormValue("password")
	data := map[string]any{}

	if username == "" || password == "" {
		data["Error"] = "Username and password are required."
		c.Status(http.StatusBadRequest)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}
	if len(username) < 3 || len(password) < 6 {
		data["Error"] = "Username must be at least 3 chars and password at least 6 chars."
		c.Status(http.StatusBadRequest)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	existing, err := h.DB.GetUserByUsername(username)
	if err != nil {
		slog.Error("registration username check error", "error", err)
		data["Error"] = "Database lookup failed."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}
	if existing != nil {
		data["Error"] = "Username is already taken."
		c.Status(http.StatusConflict)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	pwdHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("password hash error", "error", err)
		data["Error"] = "Failed to hash password."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	userID, err := h.DB.CreateUser(username, string(pwdHash))
	if err != nil {
		slog.Error("user creation error", "error", err)
		data["Error"] = "Failed to register user."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	token := h.generateSessionToken()
	expires := time.Now().Add(30 * time.Minute)
	if err = h.DB.CreateSession(token, userID, expires); err != nil {
		slog.Error("session creation error after registration", "error", err)
		data["Error"] = "Failed to initiate session."
		c.Status(http.StatusInternalServerError)
		c.SetHeader("Content-Type", "text/html; charset=utf-8")
		return h.Tmpl.ExecuteTemplate(c.Writer, "register.html", data)
	}

	c.SetCookie(http.Cookie{Name: "session_token", Value: token, Expires: expires, HttpOnly: true, Path: "/"})
	c.Redirect("/dashboard", http.StatusSeeOther)
	return nil
}

func (h *Handler) HandleLogoutPost(c *nanoserve.Context) error {
	cookie, err := c.GetCookie("session_token")
	if err == nil {
		_ = h.DB.DeleteSession(cookie.Value)
		c.SetCookie(http.Cookie{Name: "session_token", Value: "", Expires: time.Unix(0, 0), HttpOnly: true, Path: "/"})
	}
	c.Redirect("/", http.StatusSeeOther)
	return nil
}

func (h *Handler) HandleDeleteAccount(c *nanoserve.Context) error {
	user := h.mustGetUser(c)

	resumes, err := h.DB.GetResumesByUserID(user.ID)
	if err != nil {
		slog.Error("error fetching user resumes for deletion", "user_id", user.ID, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Database lookup failed.")
		return nil
	}

	if h.R2 != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		defer cancel()
		for _, resume := range resumes {
			if err = h.R2.DeleteFile(ctx, resume.R2Key); err != nil {
				slog.Warn("failed to delete R2 key during account deletion", "key", resume.R2Key, "error", err)
			}
		}
	}

	if err = h.DB.DeleteUserAndResources(user.ID); err != nil {
		slog.Error("error deleting user tables", "user_id", user.ID, "error", err)
		h.writeJSONError(c.Writer, http.StatusInternalServerError, "Failed to delete account tables.")
		return nil
	}

	c.SetCookie(http.Cookie{Name: "session_token", Value: "", Expires: time.Unix(0, 0), HttpOnly: true, Path: "/"})
	c.SetHeader("Content-Type", "application/json")
	c.Status(http.StatusOK)
	return json.NewEncoder(c.Writer).Encode(map[string]string{
		"message": "Account and all associated resources deleted successfully.",
	})
}
