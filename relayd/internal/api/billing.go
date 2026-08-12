package api

import (
	"context"
	"net/http"
	"time"
)

// DASHBOARD.md §3.6 — billing, cloud only.
//
// "Stripe's customer portal, linked rather than rebuilt. Plan, next charge,
// invoices, cancel. Do not reimplement any of it."
//
// So this is one endpoint that returns a URL. There is no plan model here, no
// invoice type, no subscription state and no webhook. Everything a customer
// wants to do about money happens on Stripe's own page, which already handles
// tax, dunning, SCA and the twelve other things a hand-rolled billing screen
// gets wrong in its second month.

// BillingPortal mints a Stripe customer portal session for one account.
//
// Cloud-only by construction: on the self-hosted tier nothing implements it,
// and the route says so in plain words rather than 404-ing as though the
// console were broken.
type BillingPortal interface {
	// PortalURL returns a short-lived URL and its expiry. The identity is passed
	// because the portal session is per-customer and this is the one place the
	// console's identity has to become a Stripe customer.
	PortalURL(ctx context.Context, id Identity) (string, time.Time, error)
}

// BillingPortalFunc adapts a function to [BillingPortal].
type BillingPortalFunc func(ctx context.Context, id Identity) (string, time.Time, error)

// PortalURL implements [BillingPortal].
func (f BillingPortalFunc) PortalURL(ctx context.Context, id Identity) (string, time.Time, error) {
	return f(ctx, id)
}

// BillingLink is the whole billing API.
type BillingLink struct {
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	// Provider is named so the console can say where it is sending you before it
	// sends you there.
	Provider string `json:"provider"`
}

func (s *Server) handleBillingPortal(w http.ResponseWriter, r *http.Request) {
	if !s.cloud || s.billing == nil {
		// Not an error the user caused, and the message says so. CLOUD.md §1:
		// there are two ways to have Relay and no middle — the free tier has no
		// bill because there is nothing of ours in the loop.
		writeErr(w, http.StatusNotFound, CodeSelfHosted,
			"this is a self-hosted Relay: there is no subscription, no bill and nothing "+
				"of ours in the loop. Billing exists on Relay Cloud only")
		return
	}

	id, _ := IdentityFrom(r.Context())
	url, expires, err := s.billing.PortalURL(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, CodeFailed,
			"could not open the billing portal: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, BillingLink{
		URL:       url,
		ExpiresAt: msOrZero(expires),
		Provider:  "stripe",
	})
}
