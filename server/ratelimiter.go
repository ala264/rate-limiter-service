package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

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

const (
	bucketCapacity = 5
	refillRate     = 0.5 // tokens per second
)

func (s *Server) handleRateLimit(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("id")
	if userID == "" {
		userID = r.RemoteAddr
	}

	evalCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := s.rdb.Eval(
		evalCtx,
		luaScript,
		[]string{"ratelimit:" + userID},
		time.Now().UnixMilli(),
		refillRate,
		bucketCapacity,
	).Result()
	redisLatency.Observe(time.Since(start).Seconds())

	if err != nil {
		redisErrors.Inc()
		requestsTotal.WithLabelValues("bypassed").Inc()
		s.logger.Warn("redis error, failing open", "error", err, "userId", userID)
		w.Header().Set("X-RateLimit-Status", "Bypassed")
		fmt.Fprintf(w, "Request Allowed (System bypass active)")
		return
	}

	if result.(int64) == 1 {
		requestsTotal.WithLabelValues("allowed").Inc()
		w.Header().Set("X-RateLimit-Status", "Allowed")
		fmt.Fprintf(w, "Request Allowed! (User: %s)", userID)
	} else {
		requestsTotal.WithLabelValues("blocked").Inc()
		w.Header().Set("X-RateLimit-Status", "Blocked")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, "Too Many Requests! Please wait 10 seconds.")
	}
}

func (s *Server) trackUniqueClients(ctx context.Context) {
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
				keys, nextCursor, err := s.rdb.Scan(ctx, cursor, "ratelimit:*", 100).Result()
				if err != nil {
					s.logger.Warn("failed to scan Redis keys for unique client count", "error", err)
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
