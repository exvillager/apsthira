package handler

import (
	"net/http"

	"github.com/exvillager/nanoserve"
)

// HandleHealth reports whether the app can serve traffic. It pings the
// database — the one dependency that would make the app unusable — so
// monitoring can distinguish "process is up" from "app actually works".
func (h *Handler) HandleHealth(c *nanoserve.Context) error {
	if err := h.DB.Ping(); err != nil {
		return c.Status(http.StatusServiceUnavailable).JSON(map[string]string{
			"status": "error",
			"error":  "database unreachable",
		})
	}
	return c.JSON(map[string]string{"status": "ok"})
}
