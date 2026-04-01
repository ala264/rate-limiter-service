package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: "",
		DB:       0,
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		logger.Error("could not connect to Redis at startup", "addr", redisAddr, "error", err)
		os.Exit(1)
	}
	logger.Info("connected to Redis", "addr", redisAddr)

	srv := NewServer(rdb, logger)

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	go srv.trackUniqueClients(clientCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleRateLimit)
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/ready", srv.handleReady)
	mux.Handle("/metrics", promhttp.Handler())

	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: metricsMiddleware(mux),
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		sig := <-quit
		logger.Info("shutdown signal received", "signal", sig.String())

		clientCancel()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", "error", err)
		}
		close(idleConnsClosed)
	}()

	logger.Info("ShieldNode starting", "port", port)
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	<-idleConnsClosed
	logger.Info("server stopped cleanly")
}
