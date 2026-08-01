package middleware

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/exvillager/nanoserve"
	"golang.org/x/time/rate"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func RequestLogger() nanoserve.HandlerFunction {
	return func(c *nanoserve.Context) error {
		// Health checks get polled frequently and add little value as logs —
		// skip them so they don't drown out real request activity.
		if c.Request.URL.Path == "/health" {
			return c.Next()
		}

		start := time.Now()

		ip, err := c.IP()
		if err != nil {
			ip = c.Request.RemoteAddr
		}
		if h, _, err := net.SplitHostPort(ip); err == nil {
			ip = h
		}

		slog.Info("-->",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"ip", ip,
		)

		rec := &statusRecorder{ResponseWriter: c.Writer, status: http.StatusOK}
		c.Writer = rec

		err = c.Next()

		slog.Info("<--",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)

		return err
	}
}

type ipLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type IPRateLimiter struct {
	ips map[string]*ipLimiterEntry
	mu  sync.RWMutex
	r   rate.Limit
	b   int
}

func NewIPRateLimiter(r rate.Limit, b int) *IPRateLimiter {
	i := &IPRateLimiter{
		ips: make(map[string]*ipLimiterEntry),
		r:   r,
		b:   b,
	}
	go i.cleanupLoop()
	return i
}

func (i *IPRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	for range ticker.C {
		i.mu.Lock()
		for ip, entry := range i.ips {
			if time.Since(entry.lastSeen) > 1*time.Hour {
				delete(i.ips, ip)
			}
		}
		i.mu.Unlock()
	}
}

func (i *IPRateLimiter) getLimiter(ip string) *rate.Limiter {
	i.mu.Lock()
	defer i.mu.Unlock()

	entry, exists := i.ips[ip]
	if !exists {
		entry = &ipLimiterEntry{
			limiter:  rate.NewLimiter(i.r, i.b),
			lastSeen: time.Now(),
		}
		i.ips[ip] = entry
	} else {
		entry.lastSeen = time.Now()
	}

	return entry.limiter
}

func RateLimit(limiter *IPRateLimiter) nanoserve.HandlerFunction {
	return func(c *nanoserve.Context) error {
		ip, err := c.IP()
		if err != nil {
			ip = c.Request.RemoteAddr
		}
		if strings.Contains(ip, ":") {
			if host, _, err := net.SplitHostPort(ip); err == nil {
				ip = host
			}
		}

		if !limiter.getLimiter(ip).Allow() {
			c.Status(http.StatusTooManyRequests)
			c.Writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(c.Writer).Encode(map[string]string{"error": "Too many requests. Please try again later."})
			c.Abort()
			return nil
		}

		return c.Next()
	}
}
