package main

import (
	"net/http"
	"strconv"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.statusCode = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		requestsInFlight.Inc()
		defer requestsInFlight.Dec()

		recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(recorder, r)

		method := r.Method
		path := r.URL.Path
		code := strconv.Itoa(recorder.statusCode)

		apiRequestsTotal.WithLabelValues(method, path, code).Inc()
		apiRequestDuration.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
	})
}
