module github.com/lloyd565/concert-ticket-reservation/services/gateway

go 1.25.0

// The shared contracts module is not published; every consumer points at the
// local path. Docker builds therefore use the repository root as their context.
replace github.com/lloyd565/concert-ticket-reservation/proto => ../../proto

require (
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/lloyd565/concert-ticket-reservation/proto v0.0.0-00010101000000-000000000000
	golang.org/x/time v0.15.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)
