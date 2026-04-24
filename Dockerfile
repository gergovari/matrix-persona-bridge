FROM golang:1.22-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Build statically with nocgo to ensure compatibility across environments
RUN CGO_ENABLED=0 go build -tags nocgo -o /matrix-persona-bridge ./cmd/matrix-persona-bridge

FROM alpine:latest
RUN apk add --no-cache ca-certificates

WORKDIR /data
COPY --from=builder /matrix-persona-bridge /usr/local/bin/matrix-persona-bridge

# The app writes/reads config.yaml and registration.yaml from the working directory
CMD ["matrix-persona-bridge"]
