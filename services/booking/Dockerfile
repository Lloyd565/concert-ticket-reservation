# syntax=docker/dockerfile:1
#
# Build context is the repository ROOT, not this directory: since P1 the service
# depends on the shared proto module through a replace directive, so that module
# has to be inside the context. See docker-compose.yml.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/go.mod proto/go.sum ./proto/
COPY services/booking/go.mod services/booking/go.sum ./services/booking/
RUN cd services/booking && go mod download
COPY proto ./proto
COPY services/booking ./services/booking
RUN cd services/booking && CGO_ENABLED=0 go build -o /out/booking ./cmd/booking

# ponytail: alpine, not distroless, so the compose healthcheck can use wget.
# Switch to distroless once the binary grows a `-healthcheck` self-probe flag.
FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/booking /usr/local/bin/booking
USER app
EXPOSE 8080 9092
ENTRYPOINT ["/usr/local/bin/booking"]
