package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ApprovalGuard is what Codex's effective config says about the two settings
// that decide whether approvals reach Relay at all.
//
// This is the pre-flight half of ADAPTERS.md §3's trap check; the live half is
// `thread/settings/updated`, which narrows a session's CapNeedsInput while it is
// running. Both exist because the failure presents as "the glasses never ask me
// anything", which reads as a feature until something destructive runs
// unattended.
type ApprovalGuard struct {
	// Policy is `approval_policy`, "" when config/read did not report one.
	Policy string
	// Reviewer is `approvals_reviewer`, "" when not reported.
	Reviewer string
	// Found is false when neither key appeared anywhere in the result. The
	// caller must treat that as "unknown", not as "fine": there is no
	// ServerResponse.json, so the result's shape is not in any contract and a
	// key we did not find may simply be somewhere we did not look.
	Found bool
	// Raw is the whole result, for the installer to show a human.
	Raw json.RawMessage
}

// OK reports whether approvals can reach us. It is false for the two settings
// that switch them off, and false when nothing was found — an unknown guard is
// not a passing guard.
func (g ApprovalGuard) OK() bool {
	if !g.Found {
		return false
	}
	if g.Policy == "never" {
		return false
	}
	return g.Reviewer == "" || g.Reviewer == "user"
}

// Why explains a failing guard in one line, for the console.
func (g ApprovalGuard) Why() string {
	switch {
	case !g.Found:
		return "codex config/read did not report approval_policy or approvals_reviewer; the result shape is outside the vendored contract, so this is unknown rather than fine"
	case g.Policy == "never":
		return `approval_policy is "never": Codex will not ask before running anything`
	case g.Reviewer != "" && g.Reviewer != "user":
		return fmt.Sprintf("approvals_reviewer is %q: a subagent decides, and the five approval requests never arrive", g.Reviewer)
	}
	return ""
}

// CheckApprovals reads Codex's effective config for a directory.
//
// `config/read`'s *result* is not in the vendored schemas — `generate-json-schema`
// emits params only — so this walks the returned tree looking for the two keys
// rather than unmarshalling into a struct that would silently produce zero
// values if the shape moved. Not finding them is reported as not finding them.
func (a *Adapter) CheckApprovals(ctx context.Context, cwd string) (ApprovalGuard, error) {
	res, err := a.c.call(ctx, "config/read", configReadParams{Cwd: cwd})
	if err != nil {
		return ApprovalGuard{}, fmt.Errorf("codex: config/read: %w", err)
	}
	g := ApprovalGuard{Raw: res}

	var tree any
	if err := json.Unmarshal(res, &tree); err != nil {
		return g, fmt.Errorf("codex: config/read returned undecodable JSON: %w", err)
	}
	if v, ok := findString(tree, "approval_policy", "approvalPolicy"); ok {
		g.Policy, g.Found = v, true
	}
	if v, ok := findString(tree, "approvals_reviewer", "approvalsReviewer"); ok {
		g.Reviewer, g.Found = v, true
	}
	if !g.OK() {
		a.log.Warn("codex: approvals may not reach Relay", "why", g.Why())
	}
	return g, nil
}

// findString walks a decoded JSON tree for the first string value under any of
// the given keys. Codex's config is nested by layer and the layout is
// undocumented here, so a search beats a path.
func findString(node any, keys ...string) (string, bool) {
	switch v := node.(type) {
	case map[string]any:
		for _, k := range keys {
			if raw, ok := v[k]; ok {
				if s, ok := raw.(string); ok {
					return s, true
				}
				// AskForApproval's other branch is `{granular:{…}}`, which is
				// not "never" and does not disable approvals.
				if m, ok := raw.(map[string]any); ok {
					for name := range m {
						return name, true
					}
				}
			}
		}
		// Deterministic order, so a config with the key in two layers reports
		// the same one every time.
		names := make([]string, 0, len(v))
		for name := range v {
			names = append(names, name)
		}
		sortStrings(names)
		for _, name := range names {
			if s, ok := findString(v[name], keys...); ok {
				return s, true
			}
		}
	case []any:
		for _, el := range v {
			if s, ok := findString(el, keys...); ok {
				return s, true
			}
		}
	}
	return "", false
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.Compare(s[j], s[j-1]) < 0; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
