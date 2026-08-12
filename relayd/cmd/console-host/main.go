// Command console-host serves the cloud console's static bundle.
//
// `DASHBOARD.md` §2 is "one web app, served from two places". This is the second
// place, and it is deliberately the smallest thing that can be: it serves files
// and nothing else. It holds no session, reads no database, and has no route
// that touches a customer's data — because `CONTROL-PLANE.md` §3 puts the
// console's traffic on a socket straight to the box through the relay, and
// putting ourselves in that path is what §3 forbids.
//
// That is what makes this component boring in the way it needs to be. If it is
// down, nobody's machine stops working and nobody's data is anywhere else; a
// customer with the page already open keeps going, because the tab is talking to
// their box, not to us. It is a file server for a page that then leaves.
//
// # Why it is not `relayd -serve-console`
//
// The cloud bundle is built with the account backend's address compiled into it
// (`console/vite.config.ts`), so it is a different artifact from the one relayd
// embeds. Everything about *how* it is served is the same code —
// `internal/web` — so the ETags, the SPA fallback and the policy cannot drift
// between the two tiers.
//
//	console-host -dir ./dist-cloud -listen :8080 \
//	  -relay wss://relay.example -accounts https://p.supabase.co
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

	"github.com/luthor007/relay/relayd/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], nil); err != nil {
		fmt.Fprintln(os.Stderr, "console-host:", err)
		os.Exit(1)
	}
}

// run is separated from main so a test can start it on port 0 and be told where
// it landed — the same shape `cmd/relayd` uses, and for the same reason.
func run(ctx context.Context, args []string, ready func(net.Addr)) error {
	fs := flag.NewFlagSet("console-host", flag.ContinueOnError)
	dir := fs.String("dir", envOr("RELAY_CONSOLE_DIR", "dist-cloud"),
		"the built cloud bundle")
	listen := fs.String("listen", envOr("RELAY_LISTEN", ":8080"),
		"address to serve on")
	relay := fs.String("relay", os.Getenv("RELAY_RELAY_ORIGIN"),
		"the rendezvous relay's origin, e.g. wss://relay.example")
	accounts := fs.String("accounts", os.Getenv("RELAY_ACCOUNTS_ORIGIN"),
		"the account service's origin, e.g. https://project.supabase.co")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// The two origins the page legitimately reaches. Refused rather than
	// defaulted: a console served with `connect-src 'self'` loads perfectly and
	// then fails to open a socket, which presents as "your machine is
	// unreachable" — a sentence about the customer's box that is really a
	// sentence about our deployment.
	if strings.TrimSpace(*relay) == "" || strings.TrimSpace(*accounts) == "" {
		return errors.New("-relay and -accounts are both required: the console must be allowed to " +
			"reach them by the content security policy, and a page that cannot is a page that " +
			"reports the customer's machine as unreachable")
	}

	console, err := web.Handler(web.Options{
		FS:         os.DirFS(*dir),
		ConnectSrc: []string{*relay, *accounts},
		Log:        log,
	})
	if err != nil {
		return err
	}
	if !console.Built() {
		return fmt.Errorf("%s holds no index.html; run `npm run build:cloud` in console/", *dir)
	}

	mux := http.NewServeMux()
	mux.Handle("/", console)
	// For the platform's health check, and it says nothing else: this process
	// has no state to report on and no customer to describe.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	if ready != nil {
		ready(ln.Addr())
	}
	log.Info("console-host: serving", "addr", ln.Addr().String(), "dir", *dir,
		"assets", len(console.Assets()))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shut)
	}
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
