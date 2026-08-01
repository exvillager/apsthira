package metrics

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/exvillager/nanoserve"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "apsthira_http_requests_total",
		Help: "Total HTTP requests processed, labeled by method, route and status.",
	}, []string{"method", "route", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "apsthira_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, labeled by method and route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Middleware records request count and latency for every request, grouped
// by a cardinality-safe route label (dynamic slugs collapse to ":slug" so
// one metric series doesn't get created per resume link).
func Middleware() nanoserve.HandlerFunction {
	return func(c *nanoserve.Context) error {
		start := time.Now()

		rec := &statusRecorder{ResponseWriter: c.Writer, status: http.StatusOK}
		c.Writer = rec

		err := c.Next()

		route := normalizeRoute(c.Request.URL.Path)
		requestsTotal.WithLabelValues(c.Request.Method, route, strconv.Itoa(rec.status)).Inc()
		requestDuration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())

		return err
	}
}

// normalizeRoute collapses the dynamic slug segment in /r/<slug>/... paths
// so metric cardinality stays fixed regardless of how many resumes exist.
func normalizeRoute(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 3 && parts[1] == "r" {
		parts[2] = ":slug"
	}
	return strings.Join(parts, "/")
}

// Handler serves /metrics on the app's own public router, but only to
// callers connecting from loopback (i.e. Prometheus running on the same
// host). It checks Request.RemoteAddr directly rather than c.IP(), since
// c.IP() trusts the X-Forwarded-For/X-Real-IP headers — trivially spoofable
// here because apsthira has no reverse proxy in front of it to strip them.
func Handler() nanoserve.HandlerFunction {
	promHandler := promhttp.Handler()
	return func(c *nanoserve.Context) error {
		if !isLoopback(c.Request.RemoteAddr) {
			c.Writer.WriteHeader(http.StatusForbidden)
			return nil
		}
		promHandler.ServeHTTP(c.Writer, c.Request)
		return nil
	}
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
