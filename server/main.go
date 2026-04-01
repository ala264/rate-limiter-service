package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	// Rate limiter decision metrics
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

	// API-level HTTP metrics (RPS, distribution by route/method/status)
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

	// Concurrency
	requestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "ratelimiter_requests_in_flight",
			Help: "Number of requests currently being processed.",
		},
	)

	// Unique clients currently tracked in Redis
	uniqueClients = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "ratelimiter_unique_clients",
			Help: "Number of unique client buckets currently in Redis.",
		},
	)
)

// ---------------------------------------------------------------------------
// Metrics middleware
// ---------------------------------------------------------------------------

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
		// Skip recording metrics for the metrics endpoint itself
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

// ---------------------------------------------------------------------------
// Background: track unique clients via Redis key scan
// ---------------------------------------------------------------------------

func trackUniqueClients(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var count int64
			var cursor uint64
			for {
				keys, nextCursor, err := rdb.Scan(ctx, cursor, "ratelimit:*", 100).Result()
				if err != nil {
					logger.Warn("failed to scan Redis keys for unique client count", "error", err)
					break
				}
				count += int64(len(keys))
				cursor = nextCursor
				if cursor == 0 {
					break
				}
			}
			uniqueClients.Set(float64(count))
		}
	}
}

// ---------------------------------------------------------------------------
// Lua script
// ---------------------------------------------------------------------------

const luaScript = `
-- KEYS[1] = bucket key
-- ARGV[1] = current timestamp (ms)
-- ARGV[2] = refill rate (tokens per second)
-- ARGV[3] = bucket capacity

local key = KEYS[1]

local now = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local capacity = tonumber(ARGV[3])

-- stored values
local data = redis.call("HMGET", key, "tokens", "timestamp")

local tokens = tonumber(data[1])
local last_refill = tonumber(data[2])

if tokens == nil then
    tokens = capacity
    last_refill = now
end

-- time passed
local delta = math.max(0, now - last_refill)

-- tokens to add
local refill = (delta / 1000) * refill_rate
tokens = math.min(capacity, tokens + refill)

-- decision
if tokens >= 1 then
    tokens = tokens - 1

    redis.call("HMSET", key,
        "tokens", tokens,
        "timestamp", now
    )

    redis.call("EXPIRE", key, math.ceil(capacity / refill_rate) * 2)

    return 1
else
    redis.call("HMSET", key,
        "tokens", tokens,
        "timestamp", now
    )

    redis.call("EXPIRE", key, math.ceil(capacity / refill_rate) * 2)

    return 0
end
`

// ---------------------------------------------------------------------------
// Globals
// ---------------------------------------------------------------------------

var (
	rdb    *redis.Client
	logger *slog.Logger
)

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func rateLimitHandler(w http.ResponseWriter, r *http.Request) {
	userId := r.URL.Query().Get("id")
	if userId == "" {
		userId = r.RemoteAddr
	}

	const capacity = 5
	const refillRate = 0.5 // tokens per second

	evalCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := rdb.Eval(
		evalCtx,
		luaScript,
		[]string{"ratelimit:" + userId},
		time.Now().UnixMilli(),
		refillRate,
		capacity,
	).Result()
	redisLatency.Observe(time.Since(start).Seconds())

	if err != nil {
		redisErrors.Inc()
		requestsTotal.WithLabelValues("bypassed").Inc()
		logger.Warn("redis error, failing open", "error", err, "userId", userId)
		w.Header().Set("X-RateLimit-Status", "Bypassed")
		fmt.Fprintf(w, "Request Allowed (System bypass active)")
		return
	}

	if result.(int64) == 1 {
		requestsTotal.WithLabelValues("allowed").Inc()
		w.Header().Set("X-RateLimit-Status", "Allowed")
		fmt.Fprintf(w, "Request Allowed! (User: %s)", userId)
	} else {
		requestsTotal.WithLabelValues("blocked").Inc()
		w.Header().Set("X-RateLimit-Status", "Blocked")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, "Too Many Requests! Please wait 10 seconds.")
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	w.Header().Set("Content-Type", "application/json")
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Error("readiness check failed", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"not ready","reason":"redis unreachable"}`)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ready"}`)
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	rdb = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: "",
		DB:       0,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		logger.Error("could not connect to Redis at startup", "addr", redisAddr, "error", err)
		os.Exit(1)
	}
	logger.Info("connected to Redis", "addr", redisAddr)

	// Start background unique client tracker
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	go trackUniqueClients(clientCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("/", rateLimitHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/ready", readyHandler)
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: metricsMiddleware(mux),
	}

	// Graceful shutdown
	idleConnsClosed := make(chan struct{})
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		sig := <-quit
		logger.Info("shutdown signal received", "signal", sig.String())

		clientCancel()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", "error", err)
		}
		close(idleConnsClosed)
	}()

	logger.Info("ShieldNode starting", "port", port)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	<-idleConnsClosed
	logger.Info("server stopped cleanly")
}
