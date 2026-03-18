FROM golang:1.21-alpine AS builder
WORKDIR /src

RUN apk add --no-cache git ca-certificates

# Copia TODO o código primeiro
COPY . .

# Resolve dependências corretamente
RUN go mod tidy

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/evaluation-service .

FROM alpine:3.20
WORKDIR /app
RUN adduser -D -u 10001 appuser
COPY --from=builder /out/evaluation-service /app/evaluation-service
USER appuser

EXPOSE 8004
ENV PORT=8004
CMD ["/app/evaluation-service"]
