package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/server"
	"github.com/thozoz/pr-review-go/pkg/version"
)

func main() {
	showVersion := flag.Bool("version", false, "Print release version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("pr-review-server %s (%s, %s)\n", version.Version, version.Commit, version.Date)
		return
	}
	cfg := config.Load()
	srv := server.NewServer(cfg)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server stopped with error: %v", err)
	}
}
