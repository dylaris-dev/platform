package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"dylaris-core/services"
	"dylaris-core/store"
)

// customDomainGate decides whether this caller may point `domain` at us.
//
// Returns a non-nil error to refuse, with a message meant for the customer.
// It only READS: arming the grant is armCustomDomainClaim, called after the
// route actually exists.
//
// ADMINS ARE EXEMPT ENTIRELY - not "checked and usually passing", but never
// checked. The proof exists to stop a tenant from claiming a domain they do not
// control; an operator configuring their own platform is the party the check
// protects, and making them wait four hours to point their own domain at their
// own edge would be ceremony with no threat behind it.
//
// It is also a no-op for anything that is not a custom domain: a subdomain of a
// hoster domain is already ours, so there is nothing to prove.
func (h *GatewayHandler) customDomainGate(r *http.Request, userID, domain string, isCustom bool) error {
	if !isCustom || IsAdmin(r) {
		return nil
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil
	}

	claim, err := h.state.Store.GetCustomDomainClaim(userID, domain)
	if err != nil && err != store.ErrNoClaim {
		// Fail CLOSED. Not knowing whether this user is blocked is not a reason
		// to assume they are not.
		return fmt.Errorf("could not check the ownership status of %s", domain)
	}
	if claim != nil && claim.State == store.ClaimPermablocked {
		return fmt.Errorf("%s is blocked for your account after %d failed ownership checks. "+
			"To unblock it, add the TXT record shown under Custom domains in your panel",
			domain, claim.Attempts)
	}
	return nil
}

// armCustomDomainClaim starts the four-hour grant and returns the instruction
// the customer needs. Call it AFTER the route exists, never before.
//
// Arming used to happen inside the gate, which runs before the port check, the
// per-account route cap and the create itself. So a request refused with
// "Route limit reached" still left a live claim on the customer's own domain,
// with a deadline they were not told about and no route to justify it. The
// verifier fails a pending claim on its deadline whether or not any route
// exists, and two failures block the domain for that account permanently - so
// two refused attempts could permanently block a domain the customer had never
// managed to route at all.
//
// Idempotent for a domain already proven (returns no notice), and
// StartCustomDomainClaim refuses to re-arm a permanent block, so a retry
// cannot launder one.
func (h *GatewayHandler) armCustomDomainClaim(r *http.Request, userID, domain string, isCustom bool) string {
	if !isCustom || IsAdmin(r) {
		return ""
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return ""
	}
	claim, err := h.state.Store.GetCustomDomainClaim(userID, domain)
	if err == nil && claim != nil && claim.State == store.ClaimVerified {
		return "" // already proven; nothing to wait for
	}
	claim, err = h.state.Store.StartCustomDomainClaim(userID, domain,
		time.Now().Add(services.CustomDomainGrant))
	if err != nil {
		// The route is already created; failing the request now would be worse
		// than an unclaimed domain, which the next create re-arms.
		log.Printf("custom-domain: could not arm the claim for %s (user %s): %v", domain, userID, err)
		return ""
	}
	// The token is the proof, so every claim carries one from the start. An
	// existing token is kept: re-minting would invalidate a record the
	// customer may already have published.
	if claim.TXTToken == "" {
		token, terr := services.NewTXTToken()
		if terr == nil {
			terr = h.state.Store.SetCustomDomainTXTToken(claim.ID, token)
		}
		if terr != nil {
			log.Printf("custom-domain: could not mint the token for %s (user %s): %v", domain, userID, terr)
			return ""
		}
		claim.TXTToken = token
	}
	hosters, _, cname := h.loadGatewayDomainConfig()
	bases := make([]string, 0, len(hosters))
	for _, hd := range hosters {
		bases = append(bases, hd.Domain)
	}
	return customDomainDeadlineHint(services.TXTVerifyPrefix+domain, claim.TXTToken,
		services.CNAMETargets(cname, bases))
}

// customDomainDeadlineHint is the instruction shown after a route on an
// unproven domain is created: the TXT record that proves ownership, and the
// record that brings players to us. Written here so the two route handlers cannot
// drift apart on what they promise the customer - which they could not do while
// it had no caller at all, and neither of them promised anything.
//
// cnameTargets are FULL names, one per region, from services.CNAMETargets. The
// operator setting alone is a label ("route") and naming it here would send the
// customer to create a record that resolves nowhere.
func customDomainDeadlineHint(txtName, txtValue string, cnameTargets []string) string {
	proof := fmt.Sprintf("Prove you own this domain within %s by adding a TXT record %s with the value %s, "+
		"or the route is removed.", formatGrant(services.CustomDomainGrant), txtName, txtValue)
	switch len(cnameTargets) {
	case 0:
		return proof + " Players reach it once the domain points at us."
	case 1:
		return proof + fmt.Sprintf(" Players reach it once it has a CNAME to %s "+
			"(or an A record to one of our edge addresses).", cnameTargets[0])
	default:
		return proof + fmt.Sprintf(" Players reach it once it has a CNAME to whichever of these is your region - %s - "+
			"(or an A record to one of our edge addresses).", strings.Join(cnameTargets, ", "))
	}
}

func formatGrant(d time.Duration) string {
	h := int(d.Hours())
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}
