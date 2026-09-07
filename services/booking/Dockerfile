# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/booking ./cmd/booking

# ponytail: alpine, not distroless, so the compose healthcheck can use wget.
# Switch to distroless once the binary grows a `-healthcheck` self-probe flag.
FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/booking /usr/local/bin/booking
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/booking"]
