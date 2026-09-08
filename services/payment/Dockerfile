# syntax=docker/dockerfile:1
#
# Build context is the repository ROOT, not this directory: the service depends
# on the shared proto module through a replace directive, so that module has to
# be inside the context. See docker-compose.yml.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/go.mod proto/go.sum ./proto/
COPY services/payment/go.mod services/payment/go.sum ./services/payment/
RUN cd services/payment && go mod download
COPY proto ./proto
COPY services/payment ./services/payment
RUN cd services/payment && CGO_ENABLED=0 go build -o /out/payment ./cmd/payment

# ponytail: alpine, not distroless, so the compose healthcheck can use wget.
# Switch to distroless once the binary grows a `-healthcheck` self-probe flag.
FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/payment /usr/local/bin/payment
USER app
EXPOSE 8082 9093
ENTRYPOINT ["/usr/local/bin/payment"]
