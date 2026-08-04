FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o alfred ./cmd/alfred/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /app/alfred /usr/local/bin/alfred

ENTRYPOINT ["alfred"]
CMD ["-config", "/etc/alfred/config.yaml", "-workflows", "/etc/alfred/workflows"]
