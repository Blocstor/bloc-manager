FROM golang:1.22-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /bloc-manager ./cmd/manager

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /bloc-manager /usr/local/bin/bloc-manager

RUN mkdir -p /var/lib/bloc-manager

EXPOSE 9090

ENTRYPOINT ["bloc-manager"]
