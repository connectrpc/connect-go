package main

import (
	"context"
	"log"
	"net/http"

	"github.com/bufbuild/connect-go"
	pingv1 "github.com/bufbuild/connect-go/internal/gen/connect/ping/v1"
	"github.com/bufbuild/connect-go/internal/gen/connect/ping/v1/pingv1connect"
)

func main() {
	client := pingv1connect.NewPingServiceClient(
		http.DefaultClient,
		"http://localhost:8080/",
	)
	_, err := client.Ping(context.Background(), connect.NewRequest(&pingv1.PingRequest{}))
	log.Println(err)
	log.Println(connect.IsServerError(err))
}
