package metrics

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type Registry struct {
	mu        sync.Mutex
	requests  map[string]uint64
	durations map[string]float64
	started   time.Time
}

func NewRegistry() *Registry {
	return &Registry{requests: make(map[string]uint64), durations: make(map[string]float64), started: time.Now()}
}

func (r *Registry) Instrument(listener string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		start := time.Now()
		writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(writer, request)
		key := listener + "\x00" + request.Method + "\x00" + strconv.Itoa(writer.status)
		r.mu.Lock()
		r.requests[key]++
		r.durations[key] += time.Since(start).Seconds()
		r.mu.Unlock()
	})
}

func (r *Registry) Handler(db *sql.DB, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/metrics" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = fmt.Fprintf(w, "# HELP cf_r2_manager_info Build information.\n# TYPE cf_r2_manager_info gauge\ncf_r2_manager_info{version=%q} 1\n", version)
		_, _ = fmt.Fprintf(w, "# HELP cf_r2_manager_uptime_seconds Process uptime.\n# TYPE cf_r2_manager_uptime_seconds gauge\ncf_r2_manager_uptime_seconds %.0f\n", time.Since(r.started).Seconds())

		r.mu.Lock()
		for key, count := range r.requests {
			listener, method, status := splitKey(key)
			_, _ = fmt.Fprintf(w, "cf_r2_manager_http_requests_total{listener=%q,method=%q,status=%q} %d\n", listener, method, status, count)
			_, _ = fmt.Fprintf(w, "cf_r2_manager_http_request_duration_seconds_sum{listener=%q,method=%q,status=%q} %f\n", listener, method, status, r.durations[key])
		}
		r.mu.Unlock()

		if db != nil {
			for _, status := range []string{"pending", "running", "succeeded", "failed"} {
				var count int64
				if err := db.QueryRowContext(request.Context(), "SELECT COUNT(*) FROM jobs WHERE status = ?", status).Scan(&count); err == nil {
					_, _ = fmt.Fprintf(w, "cf_r2_manager_jobs{status=%q} %d\n", status, count)
				}
			}
		}
	})
}

func splitKey(key string) (string, string, string) {
	parts := [3]string{}
	part := 0
	start := 0
	for index := 0; index <= len(key); index++ {
		if index == len(key) || key[index] == 0 {
			if part < len(parts) {
				parts[part] = key[start:index]
			}
			part++
			start = index + 1
		}
	}
	return parts[0], parts[1], parts[2]
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
