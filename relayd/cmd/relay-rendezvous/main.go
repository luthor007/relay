// Command relay-rendezvous is the relay from SYSTEM.md §7.
//
//	relay-rendezvous --listen :8080
//
// A Mac mini in someone's house is not reachable from a phone on cellular, and
// §7 settled the answer: we run a relay and it is free even for self-hosters.
// Both sides dial out and it pipes bytes it cannot read.
//
// It is the third of SYSTEM.md's "four things we build", and it is the smallest.
// One process, no database, no state that survives a restart, and nothing to
// configure but an address. That is not minimalism for its own sake — every
// piece of state it does not hold is a piece it cannot leak, and a relay whose
// operator can be compelled to log traffic is a relay that does not deliver the
// property it exists for.
//
// # Running it
//
// Behind a TLS terminator, on any host with a public address. It is stateless,
// so more than one may run behind a load balancer **only if the balancer is
// sticky by connection** — which every WebSocket-capable one is, since a socket
// cannot move hosts mid-stream. A box and its phone must land on the same
// instance, and they will, because both dial the same hostname and hold the
// connection open.
//
// It holds one file descriptor per connection and nothing else, so the practical
// ceiling is the process's fd limit rather than memory or CPU: §7's bandwidth
// argument is that the day's audio never crosses it, only live voice and control
// traffic, which is bursty kilobytes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/luthor007/relay/relayd/internal/rendezvous"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "relay-rendezvous:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr *os.File) error {
	fs := flag.NewFlagSet("relay-rendezvous", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		listen   = fs.String("listen", ":8080", "address to serve on")
		origins  = fs.String("origins", "", "comma-separated origin allowlist for browser connections")
		logLevel = fs.String("log-level", "info", "debug | info | warn | error")
		showVer  = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVer {
		fmt.Fprintln(stderr, version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: levelOf(*logLevel)}))

	hub := rendezvous.NewHub(rendezvous.HubOptions{
		Limits: rendezvous.DefaultLimits(),
		Log:    log,
	})

	// The origin allowlist exists for the console and for nothing else. A phone
	// sends no Origin header; a browser does, and without a list any page a user
	// visits could open a relay connection in their name. Empty means the check
	// is off, which is correct for a relay that only phones dial — and is a
	// deliberate flag rather than a default, because turning it on later
	// silently breaks the console.
	var allowed []string
	if trimmed := strings.TrimSpace(*origins); trimmed != "" {
		for _, o := range strings.Split(trimmed, ",") {
			if o = strings.TrimSpace(o); o != "" {
				allowed = append(allowed, o)
			}
		}
	}

	handler := rendezvous.NewHandler(hub, log, allowed)
	srv := &http.Server{
		Handler: handler.Routes(),
		// Deliberately no ReadTimeout or WriteTimeout: every route here is a
		// long-lived WebSocket, and both would kill a healthy connection at the
		// deadline. The equivalent protections are in the hub — idle timeout per
		// read, a frame cap, and TTLs on everything held.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	log.Info("relay-rendezvous: serving", "addr", ln.Addr().String(), "version", version,
		"origins", len(allowed))

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		// Connections are dropped rather than drained. Both sides reconnect by
		// design and a drain would hold every live pipe open for the grace
		// period, which on a relay is every customer at once.
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		log.Info("relay-rendezvous: stopping")
		return srv.Shutdown(shutdown)
	}
}

func levelOf(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
