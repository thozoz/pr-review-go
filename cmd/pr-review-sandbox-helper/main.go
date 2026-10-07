package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: pr-review-sandbox-helper <command> [options]\nCommands: relay, export, retrieve, prepare\n")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch os.Args[1] {
	case "relay":
		runRelay(ctx, os.Args[2:])
	case "export":
		runExport(ctx, os.Args[2:])
	case "retrieve":
		runRetrieve(ctx, os.Args[2:])
	case "prepare":
		runPrepare(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func runRelay(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listenAddr := fs.String("listen", "127.0.0.1:8080", "TCP listen address for loopback relay")
	socketPath := fs.String("socket", "", "Unix domain socket path to forward requests to")
	_ = fs.Parse(args)

	if *socketPath == "" {
		fmt.Fprintf(os.Stderr, "Error: -socket is required\n")
		os.Exit(1)
	}

	listener, err := sandbox.StartLoopbackRelay(ctx, *listenAddr, *socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start loopback relay: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()

	<-ctx.Done()
}

func runExport(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	gitDir := fs.String("git-dir", "", "Path to bare git repository")
	commitSHA := fs.String("commit", "", "Expected full commit SHA")
	destDir := fs.String("dest", "", "Destination directory for extracted snapshot")
	manifestPath := fs.String("manifest", "", "Optional path to write JSON manifest")
	_ = fs.Parse(args)

	if *gitDir == "" || *commitSHA == "" || *destDir == "" {
		fmt.Fprintf(os.Stderr, "Error: -git-dir, -commit, and -dest are required\n")
		os.Exit(1)
	}

	snapshot, err := sandbox.ExportGitTree(ctx, *gitDir, *commitSHA, *destDir)
	if err != nil {
		if errors.Is(err, sandbox.ErrIncompleteGitlink) || errors.Is(err, sandbox.ErrIncompleteGitLFS) {
			fmt.Fprintf(os.Stderr, "INCOMPLETE: %v\n", err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "Export failed: %v\n", err)
		os.Exit(1)
	}

	if *manifestPath != "" && snapshot != nil {
		mf := struct {
			CommitSHA string            `json:"commit_sha"`
			Manifest  map[string]string `json:"manifest"`
		}{
			CommitSHA: snapshot.CommitSHA,
			Manifest:  snapshot.Manifest,
		}
		data, err := json.MarshalIndent(mf, "", "  ")
		if err == nil {
			_ = os.WriteFile(*manifestPath, data, 0644)
		}
	}
}

func runRetrieve(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("retrieve", flag.ExitOnError)
	socketPath := fs.String("socket", "", "Unix domain socket path for git gateway")
	repoURL := fs.String("repo-url", "", "Repository URL via local relay (e.g. http://127.0.0.1:8080/owner/repo)")
	commitSHA := fs.String("commit", "", "Full commit SHA to fetch and export")
	gitDir := fs.String("git-dir", "", "Path to initialize bare git repository")
	destDir := fs.String("dest", "", "Destination directory for extracted snapshot")
	manifestPath := fs.String("manifest", "", "Optional path to write JSON manifest")
	_ = fs.Parse(args)

	if *socketPath == "" || *repoURL == "" || *commitSHA == "" || *gitDir == "" || *destDir == "" {
		fmt.Fprintf(os.Stderr, "Error: missing required arguments for retrieve\n")
		os.Exit(1)
	}

	// 1. Pick an ephemeral local port for relay
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to bind ephemeral loopback port: %v\n", err)
		os.Exit(1)
	}
	relayAddr := l.Addr().String()
	_ = l.Close()

	relayCtx, cancelRelay := context.WithCancel(ctx)
	defer cancelRelay()

	relayListener, err := sandbox.StartLoopbackRelay(relayCtx, relayAddr, *socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start loopback relay on %s: %v\n", relayAddr, err)
		os.Exit(1)
	}
	defer relayListener.Close()

	// 2. Rewrite repoURL host to relayAddr
	u, err := url.Parse(*repoURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid repo URL %s: %v\n", *repoURL, err)
		os.Exit(1)
	}
	u.Host = relayAddr
	fetchURL := u.String()

	// 3. Initialize bare git repository
	if err := os.MkdirAll(*gitDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create git dir: %v\n", err)
		os.Exit(1)
	}

	initCmd := exec.CommandContext(ctx, "git", "init", "--bare", *gitDir)
	initCmd.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TEMPLATE_DIR=/dev/null",
		"HOME=/tmp",
	}
	if out, err := initCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "git init --bare failed: %v (output: %s)\n", err, string(out))
		os.Exit(1)
	}

	// 4. Fetch the exact commit object
	fetchCmd := exec.CommandContext(ctx, "git", "-C", *gitDir, "fetch", "--depth=1", fetchURL, *commitSHA)
	fetchCmd.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TEMPLATE_DIR=/dev/null",
		"HOME=/tmp",
	}
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "git fetch failed: %v (output: %s)\n", err, string(out))
		os.Exit(1)
	}

	// 5. Export tree safely using trusted helper logic
	snapshot, err := sandbox.ExportGitTree(ctx, *gitDir, *commitSHA, *destDir)
	if err != nil {
		if errors.Is(err, sandbox.ErrIncompleteGitlink) || errors.Is(err, sandbox.ErrIncompleteGitLFS) {
			fmt.Fprintf(os.Stderr, "INCOMPLETE: %v\n", err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "Export failed: %v\n", err)
		os.Exit(1)
	}

	// 6. Write manifest if requested
	if *manifestPath != "" && snapshot != nil {
		mf := struct {
			CommitSHA string            `json:"commit_sha"`
			Manifest  map[string]string `json:"manifest"`
		}{
			CommitSHA: snapshot.CommitSHA,
			Manifest:  snapshot.Manifest,
		}
		data, err := json.MarshalIndent(mf, "", "  ")
		if err == nil {
			_ = os.WriteFile(*manifestPath, data, 0644)
		}
	}
}

func runPrepare(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	socketPath := fs.String("socket", "", "Unix domain socket path for dependency gateway")
	workDir := fs.String("work", "", "Working directory containing go.mod")
	_ = fs.Parse(args)

	if *socketPath == "" || *workDir == "" {
		fmt.Fprintf(os.Stderr, "Error: -socket and -work are required\n")
		os.Exit(1)
	}

	// 1. Validate go.mod in workDir
	if err := sandbox.ValidateGoMod(*workDir); err != nil {
		fmt.Fprintf(os.Stderr, "INCOMPLETE: %v\n", err)
		os.Exit(2)
	}

	// 2. Bind ephemeral loopback port for relay
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to bind ephemeral loopback port: %v\n", err)
		os.Exit(1)
	}
	relayAddr := l.Addr().String()
	_ = l.Close()

	relayCtx, cancelRelay := context.WithCancel(ctx)
	defer cancelRelay()

	relayListener, err := sandbox.StartLoopbackRelay(relayCtx, relayAddr, *socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start loopback relay on %s: %v\n", relayAddr, err)
		os.Exit(1)
	}
	defer relayListener.Close()

	// 3. Execute trusted go mod download with strictly sanitized environment (D-05, SAFE-01)
	downloadCmd := exec.CommandContext(ctx, "go", "mod", "download")
	downloadCmd.Dir = *workDir
	downloadCmd.Env = []string{
		"GOPROXY=" + fmt.Sprintf("http://%s", relayAddr),
		"GOSUMDB=" + fmt.Sprintf("sum.golang.org http://%s/sumdb/sum.golang.org", relayAddr),
		"GOVCS=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOENV=off",
		"GOCACHE=/cache/go-build",
		"GOMODCACHE=/cache/go/pkg/mod",
		"GOPATH=/cache/go",
		"HOME=/tmp",
		"TMPDIR=/tmp",
		"PATH=/usr/local/go/bin:/usr/bin:/bin",
	}

	out, err := downloadCmd.CombinedOutput()
	outStr := string(out)

	if err != nil {
		lowerOut := strings.ToLower(outStr)

		// Check for disk full
		if strings.Contains(lowerOut, "no space left on device") || strings.Contains(lowerOut, "enospc") {
			fmt.Fprintf(os.Stderr, "DISK_EXHAUSTED: %s\n", outStr)
			os.Exit(3)
		}

		// Check for newer toolchain requirement
		if strings.Contains(lowerOut, "cannot switch to that toolchain") ||
			strings.Contains(lowerOut, "requires go >=") ||
			strings.Contains(lowerOut, "toolchain not available") ||
			strings.Contains(lowerOut, "cannot find toolchain") {
			fmt.Fprintf(os.Stderr, "INCOMPLETE: toolchain mismatch: %s\n", outStr)
			os.Exit(2)
		}

		// Check for checksum error or missing checksum
		if strings.Contains(lowerOut, "checksum mismatch") ||
			strings.Contains(lowerOut, "security error") ||
			strings.Contains(lowerOut, "missing go.sum entry") ||
			strings.Contains(lowerOut, "not in go.sum") {
			fmt.Fprintf(os.Stderr, "INCOMPLETE: checksum verification failed: %s\n", outStr)
			os.Exit(2)
		}

		// Check for private module or repository access denied
		if strings.Contains(lowerOut, "404 not found") ||
			strings.Contains(lowerOut, "403 forbidden") ||
			strings.Contains(lowerOut, "reading https://proxy.golang.org") ||
			strings.Contains(lowerOut, "access denied") {
			fmt.Fprintf(os.Stderr, "INCOMPLETE: private module or not found on proxy: %s\n", outStr)
			os.Exit(2)
		}

		fmt.Fprintf(os.Stderr, "go mod download failed: %v (output: %s)\n", err, outStr)
		os.Exit(1)
	}
}

