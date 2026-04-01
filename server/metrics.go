package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ratelimiter_requests_total",
			Help: "Total number of rate limiter decisions, labeled by status.",
		},
		[]string{"status"}, // "allowed" | "blocked" | "bypassed"
	)

	redisLatency = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "ratelimiter_redis_latency_seconds",
			Help:    "Latency of Redis Eval calls in seconds.",
			Buckets: prometheus.DefBuckets,
		},
	)

	redisErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "ratelimiter_redis_errors_total",
			Help: "Total number of Redis errors (triggers fail-open).",
		},
	)

	apiRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "api_requests_total",
			Help: "Total API requests, labeled by method, path, and status code.",
		},
		[]string{"method", "path", "status_code"},
	)

	apiRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "api_request_duration_seconds",
			Help:    "End-to-end HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	requestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "ratelimiter_requests_in_flight",
			Help: "Number of requests currently being processed.",
		},
	)

	uniqueClients = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "ratelimiter_unique_clients",
			Help: "Number of unique client buckets currently in Redis.",
		},
	)
)
