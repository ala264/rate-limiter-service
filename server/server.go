package main

import (
	"log/slog"

	"github.com/redis/go-redis/v9"
)

type Server struct {
	rdb    *redis.Client
	logger *slog.Logger
}

func NewServer(rdb *redis.Client, logger *slog.Logger) *Server {
	return &Server{
		rdb:    rdb,
		logger: logger,
	}
}
