package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"dylaris-core/handlers/gate"
	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// customDomainGate decides whether this caller may point `domain` at us.
//
// Returns a non-nil error to refuse, with a message meant for the customer.
//
// OWNERSHIP FIRST, then the route. It used to let an unproven domain through
// and arm a four-hour window afterwards, so the route was LIVE before anything
// was proven: a domain that already pointed at us (a CNAME left behind by its
// last user) served whoever claimed it for up to four hours, and a claim that
// failed to store left the route up with nothing ever checking it. Now a
// domain without a verified claim gets a claim with no deadline (nothing runs
// out while the customer sets up DNS, no strike is counted, and the verifier
// proves it as soon as the record appears), the record is checked right here
// so a customer who has just published it is not sent round again, and
// otherwise the answer says what to publish.
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
	if !isCustom || reqctx.IsAdmin(r) {
		return nil
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil
	}

	claim, err := h.d.Store.GetCustomDomainClaim(userID, domain)
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
	if claim != nil && claim.State == store.ClaimVerified {
		return nil
	}

	claim, err = h.d.Store.RequestCustomDomainClaim(userID, domain)
	if err != nil || claim == nil {
		return fmt.Errorf("could not start the ownership check for %s", domain)
	}
	if claim.TXTToken == "" {
		token, terr := services.NewTXTToken()
		if terr == nil {
			terr = h.d.Store.SetCustomDomainTXTToken(claim.ID, token)
		}
		if terr != nil {
			log.Printf("custom-domain: could not mint the token for %s (user %s): %v", domain, userID, terr)
			return fmt.Errorf("could not start the ownership check for %s", domain)
		}
		claim.TXTToken = token
	}
	res := h.txtResolver
	if res == nil {
		res = services.NewNetResolver()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if services.CheckTXTToken(ctx, res, domain, claim.TXTToken) {
		if err := h.d.Store.MarkCustomDomainVerified(claim.ID); err != nil {
			return fmt.Errorf("could not record the ownership of %s", domain)
		}
		return nil
	}
	return errors.New(customDomainProveFirst(services.TXTVerifyPrefix+domain, claim.TXTToken))
}

// remoteOwnDomainGate is customDomainGate on the managed gateway, for a route
// on an own domain, and reports whether it answered (refused). Two proofs:
//
//  1. the server owner's verified LOCAL claim: which user of this platform
//     controls the domain. Every user here shares one provider account, so
//     the provider cannot tell them apart; a route for user B's server needs
//     B's own proof, never A's.
//  2. the PROVIDER's verified claim: that this platform controls it.
//
// An admin skips the local proof only for a server without an owner (the
// operator's own). A tenant's server needs the tenant's claim whoever creates
// the route, or the reconcile would remove it again.
//
// Unlike customDomainGate it never starts a claim: a claim made here would
// have no provider claim beside it. Adding the domain under Your own domains makes
// both. No refusal names a TXT value; the records are shown there.
func (h *GatewayHandler) remoteOwnDomainGate(w http.ResponseWriter, r *http.Request, rg *services.RemoteGateway, srv *models.Server, account, domain string) bool {
	refuse := func(status int, code, msg string) bool {
		gate.WriteGatewayError(w, status, code, msg)
		return true
	}
	// The proof is the server owner's: a collaborator or an admin acting on a
	// tenant's server cannot add it under their own domains.
	caller, _ := r.Context().Value("userID").(string)
	byOwner := srv.OwnerID == "" || srv.OwnerID == caller
	ownerFirst := "The owner of this server has to add and verify " + domain + " under their own domains first."
	if srv.OwnerID != "" || !reqctx.IsAdmin(r) {
		claim, err := h.d.Store.GetCustomDomainClaim(account, domain)
		if err != nil && err != store.ErrNoClaim {
			// Fail closed, as customDomainGate does.
			return refuse(http.StatusInternalServerError, "internal", "Could not check the ownership of "+domain+". Try again.")
		}
		switch {
		case claim == nil && !byOwner:
			return refuse(http.StatusForbidden, "domain_unverified", ownerFirst)
		case claim == nil:
			return refuse(http.StatusForbidden, "domain_unverified",
				"Add "+domain+" under Your own domains first, publish its TXT records and check it there.")
		case claim.State == store.ClaimPermablocked && !byOwner:
			return refuse(http.StatusForbidden, "domain_blocked", fmt.Sprintf(
				"%s is blocked for the owner of this server after %d failed ownership checks. The owner has to check it again under their own domains.", domain, claim.Attempts))
		case claim.State == store.ClaimPermablocked:
			return refuse(http.StatusForbidden, "domain_blocked", fmt.Sprintf(
				"%s is blocked for your account after %d failed ownership checks. Check it again under Your own domains.", domain, claim.Attempts))
		case claim.State != store.ClaimVerified:
			// The record may have just been published: checked here as on the
			// local path, so the customer is not sent round again.
			res := h.txtResolver
			if res == nil {
				res = services.NewNetResolver()
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			found := claim.TXTToken != "" && services.CheckTXTToken(ctx, res, domain, claim.TXTToken)
			cancel()
			if !found && !byOwner {
				return refuse(http.StatusForbidden, "domain_unverified", ownerFirst)
			}
			if !found {
				return refuse(http.StatusForbidden, "domain_unverified",
					"This platform has not verified "+domain+" yet. Publish its TXT record shown under Your own domains, then check again.")
			}
			if err := h.d.Store.MarkCustomDomainVerified(claim.ID); err != nil {
				return refuse(http.StatusInternalServerError, "internal", "Could not record the ownership of "+domain+". Try again.")
			}
		}
	}
	claims, err := rg.ProviderCustomDomains(r.Context())
	if gate.GatewayProviderError(w, err) {
		return true
	}
	if err != nil {
		return refuse(http.StatusInternalServerError, "internal", "Could not check the ownership of "+domain+". Try again.")
	}
	switch pc, ok := claims[domain]; {
	case pc.State == store.ClaimPermablocked && !byOwner:
		return refuse(http.StatusForbidden, "domain_blocked",
			"The gateway provider blocked "+domain+" after failed ownership checks. The owner of this server has to check it again under their own domains.")
	case (!ok || pc.State != store.ClaimVerified) && !byOwner:
		return refuse(http.StatusForbidden, "domain_unverified", ownerFirst)
	case !ok:
		return refuse(http.StatusForbidden, "domain_unverified",
			"The gateway provider has no record for "+domain+" yet. Check the domain again under Your own domains to get it.")
	case pc.State == store.ClaimPermablocked:
		return refuse(http.StatusForbidden, "domain_blocked",
			"The gateway provider blocked "+domain+" after failed ownership checks. Check it again under Your own domains.")
	case pc.State != store.ClaimVerified:
		return refuse(http.StatusForbidden, "domain_unverified",
			"The gateway provider has not verified "+domain+" yet. Publish its TXT record shown under Your own domains, then check again.")
	}
	return false
}

// customDomainProveFirst is the refusal for a domain not proven yet.
func customDomainProveFirst(txtName, txtValue string) string {
	return fmt.Sprintf("Prove you own this domain first: add a TXT record %s with the value %s, "+
		"then create the route again. DNS changes can take a few minutes to show.", txtName, txtValue)
}

// armCustomDomainClaim runs AFTER the route exists and returns the ownership
// instruction for a domain that is still unproven. Since customDomainGate runs
// before every create and refuses a non-admin route on a domain without a
// verified claim or a live TXT match, a route that got this far is on a proven
// domain (or was made by an admin), so today this returns "" at the
// ClaimVerified check below. The arming path is kept as a safety net, not as a
// grant: there is no longer a window in which an unproven custom domain serves
// players.
//
// It must never move back before the create: a claim armed for a request that
// is then refused (route cap, port check) would carry a deadline the customer
// was never told about, and MaxClaimAttempts failed claims block the domain for
// that account.
//
// StartCustomDomainClaim refuses to re-arm a permanent block, so a retry
// cannot launder one.
func (h *GatewayHandler) armCustomDomainClaim(r *http.Request, userID, domain string, isCustom bool) string {
	if !isCustom || reqctx.IsAdmin(r) {
		return ""
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return ""
	}
	claim, err := h.d.Store.GetCustomDomainClaim(userID, domain)
	if err == nil && claim != nil && claim.State == store.ClaimVerified {
		return "" // already proven; nothing to wait for
	}
	claim, err = h.d.Store.StartCustomDomainClaim(userID, domain,
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
			terr = h.d.Store.SetCustomDomainTXTToken(claim.ID, token)
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
