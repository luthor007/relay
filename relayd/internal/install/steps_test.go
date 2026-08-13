package install

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
)

// Doctor builds its own Options and never sets HTTPClient, so the bus check has
// to survive a nil one. It did not, and panicked on the first real `relay
// doctor` run against a configured bus — a nil *http.Client segfaults rather
// than erroring, and every other caller in this package happens to be handed a
// client, which is why nothing else had ever found it.
func TestTheBusCheckSurvivesADoctorWithNoHTTPClient(t *testing.T) {
	// Listen must be set: withDefaults replaces the WHOLE Config when Listen is
	// empty, so an Options carrying only a Bus loses it silently.
	opts := Options{Config: config.Config{
		Listen: "127.0.0.1:8787", Bus: config.Bus{URL: "ws://127.0.0.1:1"}}}
	h := checkBus(context.Background(), opts.withDefaults())
	if !h.Configured {
		t.Error("a configured bus was reported as absent")
	}
	if h.Live {
		t.Error("nothing is listening on port 1, so it cannot be live")
	}
}

// An unconfigured bus is a supported state, not a fault. A doctor that reports
// a deliberate choice as broken teaches people to ignore it.
func TestAnAbsentBusIsNotAFault(t *testing.T) {
	h := checkBus(context.Background(), Options{}.withDefaults())
	if h.Configured || !h.OK() {
		t.Errorf("h = %+v, want an absent bus that is OK", h)
	}
	if !strings.Contains(h.Line(), "directly") {
		t.Errorf("the row should say what happens instead, got %q", h.Line())
	}
}

// The socket is ws://; health is http:// on the same host. Getting that wrong
// means doctor reports a live Gateway as dead.
func TestTheHealthURLIsDerivedFromTheSocketURL(t *testing.T) {
	for in, wantHost := range map[string]string{
		"ws://127.0.0.1:19311":  "127.0.0.1:19311",
		"wss://box.example:443": "box.example:443",
	} {
		opts := Options{Config: config.Config{Listen: "127.0.0.1:8787", Bus: config.Bus{URL: in}}}
		h := checkBus(context.Background(), opts.withDefaults())
		if !h.Configured || h.URL != in {
			t.Errorf("checkBus(%q) reported %+v", in, h)
		}
		if !strings.Contains(h.URL, wantHost) {
			t.Errorf("host lost from %q", in)
		}
	}
}
