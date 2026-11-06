// Command agemud is the agemu device daemon. It owns the platform backends
// (Android emulator, iOS Simulator) and serves the agemu CLI and MCP server over a
// unix socket. The agemu CLI starts it on demand from the same directory, so it
// rarely needs to be run by hand:
//
//	agemud            run in the foreground (logs to stderr)
//
// Build tags noandroid / noios compile a platform out.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tmp := filepath.Join(daemon.Home(), "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	svc := daemon.NewService(tmp, backends()...)
	defer svc.Close()
	if err := daemon.Serve(ctx, svc); err != nil {
		log.Fatal(err)
	}
}
