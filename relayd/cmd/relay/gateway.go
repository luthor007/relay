package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/install"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// gatewayProbeTimeout bounds the liveness check. It is a loopback request to a
// process that is either running or not, so a second is generous.
const gatewayProbeTimeout = time.Second

// gatewayIfLive returns the gateway descriptor, but only if the gateway is
// actually answering.
//
// This is the guard the whole MCP write half has been waiting behind, and it is
// worth being precise about why it is a probe rather than a config read.
// Adoption rewrites five runtimes' `mcp.json` to point at one server. If that
// server is not there, the user does not get a degraded Relay — they get five
// agents with no tools at all, which is a worse machine than the one they
// started with and a failure they will attribute to whichever agent they open
// first. MEMORY.md §7 calls that out and it is why install.Options.Gateway was
// left zero rather than filled in from the config.
//
// So the rule is: the endpoint we write is one we have just spoken MCP to. A
// config value is a plan; a completed initialize is a fact.
func gatewayIfLive(ctx context.Context, cfg config.Config, out io.Writer) install.MCPGateway {
	base := listenURL(cfg.Listen)
	if base == "" {
		return install.MCPGateway{}
	}
	d := mcp.HTTPDescriptor("relay", base)

	if err := probeGateway(ctx, d.URL); err != nil {
		// Not an error the caller has to handle: an installer run on a machine
		// where relayd is not up should still do everything else. But it is
		// said out loud, because "my tools did not appear" is otherwise a
		// silent outcome.
		fmt.Fprintf(out, "  mcp        gateway not reachable at %s (%v)\n", d.URL, err)
		fmt.Fprintf(out, "             leaving the five runtimes' servers as they are; "+
			"start relayd and run this again to share them\n")
		return install.MCPGateway{}
	}
	return d.Install()
}

// listenURL turns a listen address into something a runtime on this machine can
// dial.
//
// A wildcard host is the interesting case: relayd listening on :8787 or
// 0.0.0.0:8787 is reachable at 127.0.0.1:8787 from here, and that is the
// address to write — the gateway is loopback-only by policy, so a runtime
// config naming the machine's LAN address would be pointing at something that
// answers 403.
func listenURL(listen string) string {
	if strings.TrimSpace(listen) == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if port == "" || port == "0" {
		// Port 0 means "whatever the OS gives me", which is not something that
		// can be written down in advance.
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// probeGateway completes an MCP initialize against the endpoint.
//
// A TCP connect would prove something is listening; it would not prove that the
// thing listening is our gateway rather than an unrelated server that happens to
// hold the port. Since the cost of being wrong is five broken agents, this does
// the handshake and checks that the server names itself.
func probeGateway(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, gatewayProbeTimeout)
	defer cancel()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": mcp.ProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "relay-setup", "version": "1"},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}

	var out struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return fmt.Errorf("not an MCP server: %w", err)
	}
	if out.Error != nil {
		return fmt.Errorf("the server refused initialize: %s", out.Error.Message)
	}
	if out.Result.ServerInfo.Name == "" {
		return fmt.Errorf("the server did not name itself")
	}
	return nil
}
