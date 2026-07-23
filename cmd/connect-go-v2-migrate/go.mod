module github.com/connectrpc/v2/connect-go/cmd/connect-go-v2-migrate

go 1.26.4

require connectrpc.com/connect/v2 v2.0.0-00010101000000-000000000000

require (
	github.com/rogpeppe/go-internal v1.14.1
	golang.org/x/sys v0.46.0 // indirect
)

require (
	golang.org/x/mod v0.37.0
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/tools v0.47.0
)

replace connectrpc.com/connect/v2 => ../../
