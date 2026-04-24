FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev olm-dev

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /matrix-persona-bridge ./cmd/matrix-persona-bridge

FROM alpine:latest
RUN apk add --no-cache ca-certificates

WORKDIR /data
COPY --from=builder /matrix-persona-bridge /usr/local/bin/matrix-persona-bridge

# The app writes/reads config.yaml and registration.yaml from the working directory
ENTRYPOINT ["/usr/local/bin/matrix-persona-bridge"]
