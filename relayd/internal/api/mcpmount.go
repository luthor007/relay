package api

import (
	"net"
	"net/http"

	"github.com/luthor007/relay/relayd/internal/mcp"
)

// GatewayPrefix is where the shared tool bus mounts. It is mcp.HTTPPrefix
// rather than a second copy of the string, because the installer writes this
// path into five runtimes' configs and a drift between the two would be
// discovered by every agent on the machine at once.
const GatewayPrefix = mcp.HTTPPrefix

// mountGateway serves the MCP gateway, to loopback callers only.
//
// The gateway is the one endpoint on this server that cannot carry the API
// token. A runtime reaches it from the `mcp.json` Relay writes, and that file's
// HTTP entry is `{"type":"http","url":...}` on four of the five runtimes and
// `{"type":"remote","url":...}` on OpenCode — no header field on any of them.
// So the authentication here is the network boundary plus the grant list: a
// process on this machine may connect, and it still sees nothing until a human
// has granted a connector, because [mcp.DenyAll] is the default and every call
// goes through [mcp.Grants].
//
// Which makes the loopback check load-bearing rather than defensive. `--lan` is
// a deliberate decision about the console, where a token still stands between a
// stranger and the machine; it is not a decision to put the whole tool bus on
// the network unauthenticated, and nobody typing that flag is agreeing to the
// second thing. Local runtimes keep working on a LAN-exposed daemon; remote
// callers get 403 and a sentence saying why.
func (s *Server) mountGateway(mux *http.ServeMux) {
	if s.gateway == nil {
		return
	}
	h := s.gateway
	mux.Handle(GatewayPrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackRequest(r) {
			http.Error(w,
				"the MCP tool bus is loopback-only: it carries every granted connector "+
					"and cannot carry the API token, so it is not exposed by --lan",
				http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	}))
}

// loopbackRequest reports whether the caller is on this machine.
//
// It reads RemoteAddr and nothing else. An X-Forwarded-For here would be a
// header a caller controls being asked whether the caller is trustworthy, and
// relayd is not behind a proxy it configured.
func loopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A Unix socket or an httptest server with no address. Neither is
		// reachable from another machine.
		return host == "" || host == "@"
	}
	return ip.IsLoopback()
}
