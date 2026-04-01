# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o shieldnode .

# ---- runtime stage ----
FROM alpine:3.19

RUN apk --no-cache add ca-certificates wget

WORKDIR /app

COPY --from=builder /app/shieldnode .

EXPOSE 3000

ENTRYPOINT ["./shieldnode"]
