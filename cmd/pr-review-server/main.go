package main

import (
	"log"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/server"
)

func main() {
	cfg := config.Load()
	srv := server.NewServer(cfg)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server stopped with error: %v", err)
	}
}
