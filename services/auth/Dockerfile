# syntax=docker/dockerfile:1
#
# Build context is the repository ROOT, not this directory: the service depends
# on the shared proto module through a replace directive, so that module has to
# be inside the context. See docker-compose.yml.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/go.mod proto/go.sum ./proto/
COPY services/auth/go.mod services/auth/go.sum ./services/auth/
RUN cd services/auth && go mod download
COPY proto ./proto
COPY services/auth ./services/auth
RUN cd services/auth && CGO_ENABLED=0 go build -o /out/auth ./cmd/auth

# ponytail: alpine, not distroless, so the compose healthcheck can use wget.
# Switch to distroless once the binary grows a `-healthcheck` self-probe flag.
FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/auth /usr/local/bin/auth
USER app
EXPOSE 8081 9091
ENTRYPOINT ["/usr/local/bin/auth"]
