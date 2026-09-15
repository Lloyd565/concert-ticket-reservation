# syntax=docker/dockerfile:1
#
# Build context is the repository root, like every other service's, even though
# Notification does not depend on the proto module: one convention for every
# build in docker-compose.yml.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY services/notification/go.mod services/notification/go.sum ./services/notification/
RUN cd services/notification && go mod download
COPY services/notification ./services/notification
RUN cd services/notification && CGO_ENABLED=0 go build -o /out/notification ./cmd/notification

# ponytail: alpine, not distroless, so the compose healthcheck can use wget.
# Switch to distroless once the binary grows a `-healthcheck` self-probe flag.
FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/notification /usr/local/bin/notification
USER app
EXPOSE 8083
ENTRYPOINT ["/usr/local/bin/notification"]
