# syntax=docker/dockerfile:1
#
# Build context is the repository ROOT, not this directory: the service depends
# on the shared proto module through a replace directive, so that module has to
# be inside the context. See docker-compose.yml.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/go.mod proto/go.sum ./proto/
COPY services/gateway/go.mod services/gateway/go.sum ./services/gateway/
RUN cd services/gateway && go mod download
COPY proto ./proto
COPY services/gateway ./services/gateway
RUN cd services/gateway && CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway

# ponytail: alpine, not distroless, so the compose healthcheck can use wget.
# Switch to distroless once the binary grows a `-healthcheck` self-probe flag.
FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/gateway /usr/local/bin/gateway
USER app
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/gateway"]
