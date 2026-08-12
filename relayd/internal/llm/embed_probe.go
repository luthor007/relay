package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Probing an embedder — ORCHESTRATOR.md §2's rule, applied to the one
// credential whose failure is silent.
//
// Every other probe in this system answers "does this key work". An embedder
// has to answer three questions, because two of the three failures do not look
// like failures:
//
//  1. Does it answer at all? Same reason codes as everything else.
//  2. **Is it the right width?** A vec0 column's width is fixed when the table
//     is created. A 384-dimension model cannot write to a 768-wide index and
//     cannot query it. Without this assertion that surfaces at the first search
//     after a two-hour backfill, as an empty dense half — so the probe names the
//     model and both numbers, at setup, before anything has been embedded.
//  3. **Is the vector real?** A model that has not finished loading, or a proxy
//     answering with a stub, returns the right number of zeros. Zeros are
//     equidistant from every query, so the dense half silently returns the same
//     arbitrary documents forever. That is worse than an outage, because an
//     outage is visible.

// Two reason codes beyond the five in ORCHESTRATOR.md §2, both specific to
// embedders and both added for the same reason `unavailable` was: the existing
// codes cannot express these, and flattening them into `unavailable` would send
// somebody to check their network when the actual problem is that they picked a
// 384-dimension model. Only an embedder can answer correctly and still be
// unusable, which is why these are not on the chat-completion list.
const (
	// ReasonWrongWidth means the model answered, with a vector of the wrong
	// size. The credential is fine; the model is unusable against this index and
	// always will be, because a vec0 column's width is fixed at create time.
	ReasonWrongWidth Reason = "wrong_width"
	// ReasonDegenerate means the model answered with the right width and no
	// information — all zeros, or components that are not numbers. Cosine over
	// it is meaningless and it poisons every neighbour query it takes part in.
	ReasonDegenerate Reason = "degenerate_vector"
)

// EmbedReasons lists every reason code an embedding probe can return: the five
// shared ones plus the two above.
func EmbedReasons() []Reason {
	return append(Reasons(), ReasonWrongWidth, ReasonDegenerate)
}

// EmbedProbeText is the known string that gets embedded. It is ordinary prose
// rather than a single token, because some models return a degenerate vector
// for a one-character input and that would make this check lie in the safe
// direction.
const EmbedProbeText = "Relay memory probe: the payments branch, Tuesday afternoon."

// normTolerance is how far from unit length a provider's own vector may be
// before we stop calling it pre-normalised. It is a report, not a gate:
// [Embedder.Embed] normalises regardless.
const normTolerance = 1e-3

// EmbedCheck is what one real call found out.
type EmbedCheck struct {
	Provider string
	Model    string
	// Local is true for the on-machine runtime, which changes the advice for a
	// connection failure — "is it running" rather than "is your key good".
	Local bool

	Reason Reason
	// Detail is the provider's own error, verbatim where we have it
	// (ORCHESTRATOR.md §2: report what the provider actually says).
	Detail string

	// Dims is the width that came back; WantDims is the index's.
	Dims     int
	WantDims int
	// Norm is the L2 norm of the vector as the provider sent it, before we
	// normalised anything. Providers differ on whether they pre-normalise and
	// this records which one this is.
	Norm float64
	// PreNormalised reports whether the provider already returns unit vectors.
	PreNormalised bool

	Latency time.Duration
	At      time.Time
	// Ref is the credential reference tried, never the secret. Empty is normal
	// for the local runtime.
	Ref string
}

// OK reports a verified, usable embedder.
func (c EmbedCheck) OK() bool { return c.Reason == ReasonOK }

// String is the line the installer prints.
func (c EmbedCheck) String() string {
	head := c.Model
	if c.Provider != "" {
		head = fmt.Sprintf("%s on %s", c.Model, c.Provider)
	}
	if c.OK() {
		s := fmt.Sprintf("%s: ok — %d dimensions in %s", head, c.Dims, c.Latency.Round(time.Millisecond))
		if c.PreNormalised {
			s += ", already unit length"
		}
		return s
	}
	s := fmt.Sprintf("%s: %s", head, c.Reason)
	if c.Detail != "" {
		s += " — " + c.Detail
	}
	return s
}

// Advice is what to do about a failure, in one sentence. Empty when the probe
// passed or when there is nothing useful to add beyond the provider's own
// words.
func (c EmbedCheck) Advice() string {
	switch c.Reason {
	case ReasonOK:
		return ""
	case ReasonWrongWidth:
		return fmt.Sprintf("Pick a %d-dimension model — %s is the default — or start a new index at this model's width.",
			c.WantDims, DefaultLocalEmbedModel)
	case ReasonDegenerate:
		if c.Local {
			return "The model answered but said nothing. Check it finished pulling, and that the daemon is not still loading it into memory."
		}
		return "The provider answered with an empty vector. Check the model id is an embedding model and not a chat model."
	case ReasonMissingCredential:
		return "Add a credential reference — env:, file:, exec: or vault: — and run setup again."
	case ReasonUnresolvedRef:
		return "The reference is there but leads nowhere: the variable is unset, the file is gone, or the command failed."
	case ReasonExpired:
		return "The provider rejected the credential. Rotate it, or check the account is in good standing."
	case ReasonUnavailable:
		if c.Local {
			return fmt.Sprintf("Nothing answered on the local endpoint. Check the runtime is running, and that %s has been pulled.", c.Model)
		}
		return "The credential resolved and the provider answered, but not with an embedding."
	}
	return ""
}

// probe makes one real call and checks all three things.
//
// It calls the provider directly rather than going through
// [embedCore.embed], because embed normalises and width-checks on the way
// through — and the whole job here is to report the raw width and the raw norm
// rather than to quietly repair them.
func (c *embedCore) probe(ctx context.Context, call batchCall) EmbedCheck {
	chk := EmbedCheck{
		Provider: string(c.cfg.Provider),
		Model:    c.cfg.Model,
		Local:    c.cfg.Provider == EmbedOllama,
		WantDims: c.dims(),
		At:       time.Now(),
		Ref:      c.cfg.Credential.String(),
	}
	if strings.TrimSpace(c.cfg.Model) == "" {
		chk.Reason = ReasonUnavailable
		chk.Detail = ErrNoEmbedModel.Error()
		return chk
	}

	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	start := time.Now()
	vecs, err := call(ctx, []string{EmbedProbeText})
	chk.Latency = time.Since(start)

	if err != nil {
		chk.Reason, chk.Detail = classify(err)
		if chk.Reason == ReasonOK {
			// classify only returns ok for a nil error; belt and braces, because
			// an "ok" with no vector behind it is the exact lie this exists to
			// prevent.
			chk.Reason = ReasonUnavailable
			chk.Detail = err.Error()
		}
		return chk
	}
	if len(vecs) != 1 || vecs[0] == nil {
		chk.Reason = ReasonUnavailable
		chk.Detail = fmt.Sprintf("asked for one vector and got %d", len(vecs))
		return chk
	}

	v := vecs[0]
	chk.Dims = len(v)

	// 2. The width. This is the assertion the whole step exists for.
	if chk.Dims != chk.WantDims {
		chk.Reason = ReasonWrongWidth
		chk.Detail = fmt.Sprintf(
			"%s returned %d dimensions and the index is %d wide. A vec0 column's width is "+
				"fixed when the table is created, so this model cannot write to this index or "+
				"query it",
			c.cfg.Model, chk.Dims, chk.WantDims)
		return chk
	}

	// 3. The vector. Right width, no information.
	if !finite(v) {
		chk.Reason = ReasonDegenerate
		chk.Detail = fmt.Sprintf("%s returned a vector containing values that are not numbers", c.cfg.Model)
		return chk
	}
	chk.Norm = l2(v)
	if chk.Norm == 0 {
		chk.Reason = ReasonDegenerate
		chk.Detail = fmt.Sprintf(
			"%s returned %d zeros. A zero vector is the same distance from every query, so the "+
				"dense half would return arbitrary results rather than fail",
			c.cfg.Model, chk.Dims)
		return chk
	}
	if math.IsInf(chk.Norm, 0) {
		chk.Reason = ReasonDegenerate
		chk.Detail = fmt.Sprintf("%s returned a vector that cannot be normalised (norm overflows)", c.cfg.Model)
		return chk
	}
	chk.PreNormalised = math.Abs(chk.Norm-1) <= normTolerance

	chk.Reason = ReasonOK
	return chk
}

// ProbeEmbedConfig builds an embedder and probes it in one step, so a caller
// that only wants the verdict does not have to handle a construction error and
// a probe result separately.
//
// A config that cannot even be built is still a result, not an exception:
// ORCHESTRATOR.md §2 wants the installer to print what happened rather than
// abort. A width refused by the catalog comes back as dimension_mismatch, which
// is the same verdict a real call would have produced and for the same reason.
func ProbeEmbedConfig(ctx context.Context, cfg EmbedConfig) EmbedCheck {
	e, err := NewEmbedder(cfg)
	if err != nil {
		chk := EmbedCheck{
			Provider: string(cfg.Provider),
			Model:    cfg.Model,
			Local:    cfg.Provider == EmbedOllama,
			WantDims: EmbeddingDims,
			At:       time.Now(),
			Ref:      cfg.Credential.String(),
			Detail:   err.Error(),
		}
		if cfg.Dims > 0 {
			chk.WantDims = cfg.Dims
		}
		switch {
		case errors.Is(err, ErrEmbedDims):
			chk.Reason = ReasonWrongWidth
		case errors.Is(err, ErrMissingCredential):
			chk.Reason = ReasonMissingCredential
		default:
			chk.Reason = ReasonUnavailable
		}
		return chk
	}
	return e.Probe(ctx)
}
