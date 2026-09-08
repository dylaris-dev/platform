package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

type TicketsHandler struct {
	state *AppState
}

func NewTicketsHandler(state *AppState) *TicketsHandler {
	return &TicketsHandler{state: state}
}

// ── Visibility + edit-permission helpers ─────────────────────────────

// ticketVisibilityFilter returns the right store.TicketFilter for the
// given caller. Admin sees everything (no scope filter); support sees per
// the cross-team-visibility setting; everyone else sees only own tickets.
func (h *TicketsHandler) ticketVisibilityFilter(userID string, perms EffectivePermissions, settings TicketSettings) store.TicketFilter {
	if perms.IsAdmin {
		return store.TicketFilter{}
	}
	if perms.IsSupport {
		if settings.CrossTeamVisibility {
			return store.TicketFilter{}
		}
		// Restricted to team, but the team string is not on
		// EffectivePermissions - it lives on the user row (support_team) and is
		// not loaded here. Without it the team filter cannot be built, so fall
		// back to the support user's own assignments, which is the narrower and
		// therefore safe answer.
		return store.TicketFilter{AssignedUserID: &userID}
	}
	return store.TicketFilter{UserID: &userID}
}

// canSeeTicket gates GET-detail. Support + admin see everything per their
// visibility scope; users see their own; watchers always see what they were
// added to. assignedTeamMatch is precomputed by the caller.
func canSeeTicket(t *models.Ticket, perms EffectivePermissions, userID string, isWatcher bool, settings TicketSettings, supportTeam string) bool {
	if perms.IsAdmin {
		return true
	}
	if t.UserID == userID {
		return true
	}
	if isWatcher {
		return true
	}
	if perms.IsSupport {
		if settings.CrossTeamVisibility {
			return true
		}
		// Cross-team off: support sees tickets either assigned to me, or
		// matching my team, or unassigned (for triage).
		if t.AssignedUserID != nil && *t.AssignedUserID == userID {
			return true
		}
		if t.AssignedTeam == "" {
			// Unassigned — visible to every supporter for pickup.
			return true
		}
		if supportTeam != "" && t.AssignedTeam == supportTeam {
			return true
		}
	}
	return false
}

// canReply gates POST replies. Returns (allowed, mayPostInternal). Internal
// notes are support+admin only. Watchers obey their can_reply flag.
func canReply(t *models.Ticket, perms EffectivePermissions, userID string, isWatcher bool, watcherCanReply bool) (bool, bool) {
	if perms.IsAdmin || perms.IsSupport {
		return true, true
	}
	if t.UserID == userID {
		return true, false
	}
	if isWatcher && watcherCanReply {
		return true, false
	}
	return false, false
}

// canMutate gates status/priority/assignment endpoints — support+admin only.
func canMutate(perms EffectivePermissions) bool {
	return perms.IsAdmin || perms.IsSupport
}

// mayAttachServer reports whether this caller may name serverUUID on a ticket.
// The bar is the same one that decides whether they can see the server at all
// anywhere else in the panel - overview.read through the resolver, which admins
// and owners satisfy by construction - so a ticket can only ever point at a
// server its author could already open.
//
// It also hands back the server's region, because it already loaded the row and
// the ticket has to record where the server is. Asking the caller for that (the
// request used to carry a serverRegion field) let them state a region for a
// server they do not own the description of, and nothing checked it against the
// server they had just been authorised for.
//
// The boolean stays load-bearing: the caller must answer "no such server" to
// both an unknown uuid and someone else's, so a false result must not report
// which it was. The region is only meaningful when ok is true.
func (h *TicketsHandler) mayAttachServer(userID, username, serverUUID string) (string, bool) {
	if h.state == nil || h.state.Store == nil || h.state.Authz == nil {
		return "", false
	}
	srv, err := h.state.Store.GetServerByUUID(serverUUID)
	if err != nil || srv == nil {
		return "", false
	}
	perms := LoadEffectivePermissions(h.state, userID)
	res, rerr := h.state.Authz.Resolve(authz.Identity{
		UserID: userID, Username: username, IsAdmin: perms.IsAdmin,
	}, srv.ID)
	if rerr != nil {
		return "", false
	}
	if !res.HasCap("overview.read") {
		return "", false
	}
	return srv.Region, true
}

// ── Create ───────────────────────────────────────────────────────────

type createTicketRequest struct {
	CategoryID int    `json:"categoryId"`
	ServerUUID string `json:"serverUuid,omitempty"`
	// No serverRegion field on purpose. It used to be accepted here and stored
	// verbatim, which let the author name any region for a server whose region
	// the platform already knows. It is now taken from the server row.
	// What the ticket is about: "server" | "node" | "route" | "" (nothing
	// specific). SubjectRef names the node or route.
	SubjectKind  string `json:"subjectKind,omitempty"`
	SubjectRef   string `json:"subjectRef,omitempty"`
	Title        string `json:"title"`
	FirstMessage string `json:"firstMessage"`
	Priority     string `json:"priority,omitempty"`
}

// normalizeTicketSubject validates the subject pair and reduces it to a stored
// form. Anything unrecognised collapses to "nothing specific" rather than 400:
// the subject is context for whoever reads the ticket, and refusing to file a
// support request over it would block someone who is already stuck.
//
// A "server" subject carries no ref - the id lives in ServerUUID, so recording
// it twice would create two places to disagree.
func normalizeTicketSubject(kind, ref string) (string, string) {
	kind = strings.TrimSpace(strings.ToLower(kind))
	ref = strings.TrimSpace(ref)
	switch kind {
	case "server":
		return "server", ""
	case "node", "route":
		if ref == "" || len(ref) > 128 {
			return "", ""
		}
		return kind, ref
	default:
		return "", ""
	}
}

// CreateTicket POST /api/tickets - opens a ticket and records its creation in
// the ticket audit trail.
func (h *TicketsHandler) CreateTicket(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	username, _ := r.Context().Value("username").(string)
	if userID == "" {
		sendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	var req createTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.FirstMessage)
	if len(title) < 3 || len(title) > 200 {
		sendJSONError(w, "Title must be 3-200 characters", http.StatusBadRequest)
		return
	}
	if len(body) < 1 || len(body) > 10000 {
		sendJSONError(w, "First message must be 1-10000 characters", http.StatusBadRequest)
		return
	}
	cat, err := h.state.Store.GetTicketCategory(req.CategoryID)
	if err != nil || cat == nil || !cat.Enabled {
		sendJSONError(w, "Unknown or disabled category", http.StatusBadRequest)
		return
	}
	if cat.RequiresServer && strings.TrimSpace(req.ServerUUID) == "" {
		sendJSONError(w, "This category requires you to attach a server", http.StatusBadRequest)
		return
	}
	// A UUID the caller cannot reach must not be attachable. Nothing checked
	// this, so any account could file a ticket naming ANY server: the response
	// came back carrying that server's NAME and internal id (measured - a
	// stranger's UUID answered "golden-ibis-797d", id 1, while a made-up one
	// answered with neither), which made this endpoint an existence-and-name
	// oracle for anyone holding a UUID - a former member, say, whose access was
	// revoked. The row also persists: ListServersViaActiveTickets hands a
	// supporter every server named by a ticket assigned to them, joined on the
	// uuid alone with no relationship to who wrote it.
	//
	// Same answer for "no such server" and "not yours", or the fix would leave
	// the oracle in place with an extra step.
	serverRegion := ""
	if uuid := strings.TrimSpace(req.ServerUUID); uuid != "" {
		var ok bool
		serverRegion, ok = h.mayAttachServer(userID, username, uuid)
		if !ok {
			sendJSONError(w, "Server not found", http.StatusBadRequest)
			return
		}
	}
	priority := strings.TrimSpace(req.Priority)
	if priority == "" {
		priority = cat.DefaultPriority
	}
	if !validTicketPriority(priority) {
		sendJSONError(w, "Invalid priority", http.StatusBadRequest)
		return
	}

	// The ticket's region is the region of the server it is about, taken from
	// the row mayAttachServer just authorised. It used to be the constant
	// "default", described as "the user's effective region" - but a user has a
	// SET of regions (user_regions is staff visibility, not a home), so there
	// was no such value to snapshot and the column said the same thing for every
	// ticket ever filed.
	//
	// That is not cosmetic, because the column is read. The support inbox shows
	// a RegionBadge per ticket and offers a region multi-select as soon as more
	// than one region exists, filtering on exactly this field. On a two-region
	// install every ticket therefore wore a "default" badge and picking "eu" or
	// "us" emptied the list, with no selection that showed anything.
	//
	// A ticket with no server keeps "default": it is the seeded region and the
	// honest answer for a billing question that is about no server at all.
	//
	// Existing rows are deliberately NOT backfilled. The column is a snapshot of
	// where the server was when the ticket was filed, and today's servers.region
	// is not evidence about a ticket from three months ago.
	region := "default"
	if serverRegion != "" {
		region = serverRegion
	}

	subjectKind, subjectRef := normalizeTicketSubject(req.SubjectKind, req.SubjectRef)

	t := &models.Ticket{
		Region: region,
		// Both: the id for filtering and the colour, the NAME as a snapshot so
		// the ticket still says what it was filed under after the category is
		// deleted. See applyTicketCategorySnapshot.
		CategoryID:   cat.ID,
		CategoryName: cat.Name,
		UserID:       userID,
		ServerUUID:   strings.TrimSpace(req.ServerUUID),
		ServerRegion: serverRegion,
		SubjectKind:  subjectKind,
		SubjectRef:   subjectRef,
		Title:        title,
		Status:       "open",
		Priority:     priority,
		AssignedTeam: cat.DefaultAssigneeTeam,
	}
	id, err := h.state.Store.CreateTicket(t)
	if err != nil {
		sendJSONError(w, "Failed to create ticket", http.StatusInternalServerError)
		return
	}
	t.ID = id
	// First message is the user's body.
	msg := &models.TicketMessage{
		TicketID: id,
		UserID:   userID,
		Body:     body,
	}
	if _, err := h.state.Store.AddTicketMessage(msg); err != nil {
		// Non-fatal by design: the ticket row exists, so failing the request
		// would leave the user unable to tell whether anything was created.
		// But it must not be SILENT - support then opens a ticket with no body
		// and nothing anywhere says why. Log it with the id so the message can
		// be recovered or the ticket chased up.
		log.Printf("tickets: ticket %d created but its first message failed to save: %v", id, err)
	}
	actor := userID
	_ = h.state.Store.InsertTicketAudit(&models.TicketAuditEvent{
		TicketID:    id,
		EventType:   TicketEventCreated,
		ActorUserID: &actor,
		Metadata: map[string]interface{}{
			"category":     cat.Name,
			"priority":     priority,
			"server_uuid":  t.ServerUUID,
			"subject_kind": subjectKind,
			"subject_ref":  subjectRef,
		},
	})

	created, _ := h.state.Store.GetTicket(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"ticket":  created,
	})
}

// ── List ─────────────────────────────────────────────────────────────

// ListTickets GET /api/tickets — user's own tickets (with watcher includes).
// Status/priority filters via querystring.
func (h *TicketsHandler) ListMyTickets(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	filter := store.TicketFilter{
		UserID:   &userID,
		Status:   parseCSV(q.Get("status")),
		Priority: parseCSV(q.Get("priority")),
		Limit:    parseIntDefault(q.Get("limit"), 50),
		Offset:   parseIntDefault(q.Get("offset"), 0),
	}
	tickets, err := h.state.Store.ListTickets(filter)
	if err != nil {
		sendJSONError(w, "Failed to load tickets", http.StatusInternalServerError)
		return
	}
	if tickets == nil {
		tickets = []models.Ticket{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"tickets": tickets,
	})
}

// ListInboxTickets GET /api/tickets/inbox - support inbox view, gated by
// RequireCap("tickets.read") at the route (Phase 4 Task 15; the former
// in-handler "support or admin" pure gate was removed since the route now
// supersedes it). Honors cross-team-visibility. Refinable via
// status/priority/category/assigned querystring.
func (h *TicketsHandler) ListInboxTickets(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	perms := LoadEffectivePermissions(h.state, userID)
	settings := LoadTicketSettings(h.state)
	q := r.URL.Query()

	// Base filter from visibility scope. We don't apply assigned_team
	// automatically here — the support's own user row carries the team
	// string, and we fetch it for the cross-team check.
	filter := store.TicketFilter{
		Status:   parseCSV(q.Get("status")),
		Priority: parseCSV(q.Get("priority")),
		Limit:    parseIntDefault(q.Get("limit"), 50),
		Offset:   parseIntDefault(q.Get("offset"), 0),
	}
	if c := q.Get("categoryId"); c != "" {
		if n, err := strconv.Atoi(c); err == nil {
			filter.CategoryID = &n
		}
	}
	// Refine "scope" param: "mine" / "team" / "unassigned" / "all"
	scope := q.Get("scope")
	switch scope {
	case "mine":
		filter.AssignedUserID = &userID
	case "team":
		me, _ := h.state.Store.GetUserByID(userID)
		if me != nil && me.SupportTeam != "" {
			filter.AssignedTeam = me.SupportTeam
		}
	case "unassigned":
		// No store-side filter here on purpose - the post-filter below does it.
		//
		// This used to set filter.AssignedTeam to a "__unassigned__" sentinel,
		// with a comment saying the store would ideally understand it and that
		// we post-filter instead. The store does not understand it: AssignedTeam
		// goes straight into "t.assigned_team = $n", so the query asked for
		// tickets belonging to a team literally named __unassigned__. No row has
		// ever matched, and a post-filter can only narrow what the query
		// returned - so the unassigned scope came back empty for everyone,
		// admins included, and the triage queue a supporter picks work up from
		// simply had nothing in it.
	}

	tickets, err := h.state.Store.ListTickets(filter)
	if err != nil {
		sendJSONError(w, "Failed to load tickets", http.StatusInternalServerError)
		return
	}

	// Cross-team enforcement when off: post-filter to caller's visibility.
	if !perms.IsAdmin && !settings.CrossTeamVisibility && scope != "mine" {
		me, _ := h.state.Store.GetUserByID(userID)
		myTeam := ""
		if me != nil {
			myTeam = me.SupportTeam
		}
		visible := tickets[:0]
		for _, t := range tickets {
			if canSeeTicket(&t, perms, userID, false, settings, myTeam) {
				visible = append(visible, t)
			}
		}
		tickets = visible
	}

	// scope=unassigned post-filter
	if scope == "unassigned" {
		visible := tickets[:0]
		for _, t := range tickets {
			if t.AssignedUserID == nil && t.AssignedTeam == "" {
				visible = append(visible, t)
			}
		}
		tickets = visible
	}

	if tickets == nil {
		tickets = []models.Ticket{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"tickets": tickets,
	})
}

// ── Detail + reply + watchers ────────────────────────────────────────

// GetTicket GET /api/tickets/{id} - one ticket with its messages, watchers and
// audit trail. Visibility is author, watcher, or staff whose support team
// matches; anything else is 403.
func (h *TicketsHandler) GetTicket(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	perms := LoadEffectivePermissions(h.state, userID)
	settings := LoadTicketSettings(h.state)
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid ticket id", http.StatusBadRequest)
		return
	}
	t, err := h.state.Store.GetTicket(id)
	if err == sql.ErrNoRows || t == nil {
		sendJSONError(w, "Ticket not found", http.StatusNotFound)
		return
	}
	if err != nil {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}

	isWatcher, _ := h.state.Store.IsTicketWatcher(id, userID)
	myTeam := ""
	if me, err := h.state.Store.GetUserByID(userID); err == nil && me != nil {
		myTeam = me.SupportTeam
	}
	if !canSeeTicket(t, perms, userID, isWatcher, settings, myTeam) {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}

	includeInternal := perms.IsAdmin || perms.IsSupport
	messages, _ := h.state.Store.ListTicketMessages(id, includeInternal)
	if messages == nil {
		messages = []models.TicketMessage{}
	}
	watchers, _ := h.state.Store.ListTicketWatchers(id)
	if watchers == nil {
		watchers = []models.TicketWatcher{}
	}

	// Audit history is support/admin only — would expose internal-note
	// metadata to the user otherwise.
	var auditEvents []models.TicketAuditEvent
	if includeInternal {
		auditEvents, _ = h.state.Store.ListTicketAudit(id)
		if auditEvents == nil {
			auditEvents = []models.TicketAuditEvent{}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"ticket":   t,
		"messages": messages,
		"watchers": watchers,
		"audit":    auditEvents,
	})
}

type replyRequest struct {
	Body       string `json:"body"`
	IsInternal bool   `json:"isInternal,omitempty"`
}

// AddReply POST /api/tickets/{id}/messages - adds a reply and notifies the
// other participants.
func (h *TicketsHandler) AddReply(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	perms := LoadEffectivePermissions(h.state, userID)
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid ticket id", http.StatusBadRequest)
		return
	}
	t, err := h.state.Store.GetTicket(id)
	if err != nil || t == nil {
		sendJSONError(w, "Ticket not found", http.StatusNotFound)
		return
	}
	isWatcher, _ := h.state.Store.IsTicketWatcher(id, userID)
	watcherCanReply := false
	if isWatcher {
		watchers, _ := h.state.Store.ListTicketWatchers(id)
		for _, w := range watchers {
			if w.UserID == userID {
				watcherCanReply = w.CanReply
				break
			}
		}
	}
	allowed, mayInternal := canReply(t, perms, userID, isWatcher, watcherCanReply)
	if !allowed {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}

	var req replyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(req.Body)
	if len(body) < 1 || len(body) > 10000 {
		sendJSONError(w, "Reply must be 1-10000 characters", http.StatusBadRequest)
		return
	}
	if req.IsInternal && !mayInternal {
		sendJSONError(w, "Internal notes are restricted to support/admin", http.StatusForbidden)
		return
	}

	msg := &models.TicketMessage{
		TicketID:   id,
		UserID:     userID,
		Body:       body,
		IsInternal: req.IsInternal,
	}
	mid, err := h.state.Store.AddTicketMessage(msg)
	if err != nil {
		sendJSONError(w, "Failed to add reply", http.StatusInternalServerError)
		return
	}
	_ = h.state.Store.TouchTicketUpdated(id)

	// Status auto-bumps: user reply on a waiting_user ticket flips it back
	// to in_progress (or open if it was newly created and nobody picked up).
	// Support reply on an open ticket bumps to in_progress.
	if !req.IsInternal {
		switch {
		case t.UserID == userID && t.Status == "waiting_user":
			h.state.Store.UpdateTicketStatus(id, "in_progress")
		case (perms.IsSupport || perms.IsAdmin) && t.Status == "open":
			h.state.Store.UpdateTicketStatus(id, "in_progress")
		}
	}

	// Fan-out notifications. Internal notes only notify staff
	// (assignee + any support watchers), public replies notify everyone
	// involved minus the actor.
	link := "/tickets/" + strconv.Itoa(id)
	if req.IsInternal {
		EmitTicketNotification(h.state, internalNoteRecipients(h.state, t, id, userID),
			NotifyTypeTicketReply,
			"Internal note on #"+strconv.Itoa(id),
			t.Title, link)
	} else {
		recipients, _ := h.state.Store.ListTicketParticipantsForNotify(id, userID)
		EmitTicketNotification(h.state, recipients,
			NotifyTypeTicketReply,
			"New reply on #"+strconv.Itoa(id),
			t.Title, link)
	}

	msg.ID = mid
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": msg,
	})
}

// internalNoteRecipients is who may be told an internal note exists: the
// assignee, plus any watcher who is support or admin. Never the ticket owner
// and never a plain-user watcher - that is the whole point of an internal note.
// The actor is excluded, and the list is deduplicated.
//
// The watcher half was described at the call site ("assignee + any support
// watchers") from the first commit and never written: only the assignee was
// notified, so a supporter deliberately CC'd on a ticket learned nothing about
// internal notes on it.
//
// Each watcher's permissions are resolved individually rather than carried on
// the watcher row. That is one lookup per watcher, on a list that is small by
// construction and only walked when an internal note is posted - cheaper than
// widening the row and keeping a copy of a role in sync with the user table.
//
// A watcher lookup failure returns what was collected so far rather than
// nothing: the assignee still gets told. Failing the other way would make a
// database blip silently swallow the notification for everyone.
func internalNoteRecipients(state *AppState, t *models.Ticket, ticketID int, actorID string) []string {
	if state == nil || t == nil {
		return nil
	}
	seen := map[string]bool{actorID: true}
	var out []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	if t.AssignedUserID != nil {
		add(*t.AssignedUserID)
	}
	watchers, err := state.Store.ListTicketWatchers(ticketID)
	if err != nil {
		return out
	}
	for _, w := range watchers {
		if seen[w.UserID] {
			continue
		}
		if p := LoadEffectivePermissions(state, w.UserID); p.IsAdmin || p.IsSupport {
			add(w.UserID)
		}
	}
	return out
}

type statusRequest struct {
	Status string `json:"status"`
}

// UpdateStatus PATCH /api/tickets/{id}/status - changes a ticket's status and
// records the transition, from and to, in the audit trail.
func (h *TicketsHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	perms := LoadEffectivePermissions(h.state, userID)
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil || id <= 0 {
		sendJSONError(w, "Invalid id", http.StatusBadRequest)
		return
	}
	t, err := h.state.Store.GetTicket(id)
	if err != nil || t == nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	// Special case: users may close their own ticket. Support+admin may
	// move freely between any status.
	var req statusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	newStatus := strings.TrimSpace(req.Status)
	if !validTicketStatus(newStatus) {
		sendJSONError(w, "Invalid status", http.StatusBadRequest)
		return
	}
	if !canMutate(perms) {
		// User self-close-or-reopen only.
		if t.UserID != userID || (newStatus != "closed" && newStatus != "open") {
			sendJSONError(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
	if t.Status == newStatus {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	previous := t.Status
	if err := h.state.Store.UpdateTicketStatus(id, newStatus); err != nil {
		sendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	eventType := TicketEventStatusChanged
	if (previous == "closed" || previous == "resolved") && (newStatus == "open" || newStatus == "in_progress") {
		eventType = TicketEventReopened
	}
	actor := userID
	_ = h.state.Store.InsertTicketAudit(&models.TicketAuditEvent{
		TicketID:    id,
		EventType:   eventType,
		ActorUserID: &actor,
		Metadata: map[string]interface{}{
			"from": previous,
			"to":   newStatus,
		},
	})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

type priorityRequest struct {
	Priority string `json:"priority"`
}

// UpdatePriority PATCH /api/tickets/{id}/priority — support/admin only.
func (h *TicketsHandler) UpdatePriority(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	perms := LoadEffectivePermissions(h.state, userID)
	if !canMutate(perms) {
		sendJSONError(w, "Support or admin required", http.StatusForbidden)
		return
	}
	id, _ := strconv.Atoi(mux.Vars(r)["id"])
	t, err := h.state.Store.GetTicket(id)
	if err != nil || t == nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	var req priorityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	priority := strings.TrimSpace(req.Priority)
	if !validTicketPriority(priority) {
		sendJSONError(w, "Invalid priority", http.StatusBadRequest)
		return
	}
	if t.Priority == priority {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	previous := t.Priority
	if err := h.state.Store.UpdateTicketPriority(id, priority); err != nil {
		sendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	actor := userID
	_ = h.state.Store.InsertTicketAudit(&models.TicketAuditEvent{
		TicketID:    id,
		EventType:   TicketEventPriorityChanged,
		ActorUserID: &actor,
		Metadata: map[string]interface{}{
			"from": previous,
			"to":   priority,
		},
	})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

type assignRequest struct {
	AssignedUserID *string `json:"assignedUserId"`
	AssignedTeam   string  `json:"assignedTeam"`
}

// UpdateAssignment PATCH /api/tickets/{id}/assignment — support/admin only.
func (h *TicketsHandler) UpdateAssignment(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	perms := LoadEffectivePermissions(h.state, userID)
	if !canMutate(perms) {
		sendJSONError(w, "Support or admin required", http.StatusForbidden)
		return
	}
	id, _ := strconv.Atoi(mux.Vars(r)["id"])
	t, err := h.state.Store.GetTicket(id)
	if err != nil || t == nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	var req assignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	team := strings.TrimSpace(req.AssignedTeam)
	// Verify the target user (if any) exists + is admin/support.
	if req.AssignedUserID != nil {
		target, err := h.state.Store.GetUserByID(*req.AssignedUserID)
		if err != nil || target == nil {
			sendJSONError(w, "Assignee not found", http.StatusBadRequest)
			return
		}
		if !(target.IsAdmin || target.Role == "admin" || target.Role == "support") {
			sendJSONError(w, "Assignee must be support or admin", http.StatusBadRequest)
			return
		}
	}
	if err := h.state.Store.UpdateTicketAssignment(id, req.AssignedUserID, team); err != nil {
		sendJSONError(w, "Update failed", http.StatusInternalServerError)
		return
	}
	eventType := TicketEventAssigned
	if req.AssignedUserID == nil && team == "" {
		eventType = TicketEventUnassigned
	}
	meta := map[string]interface{}{"team": team}
	if req.AssignedUserID != nil {
		meta["assigned_user_id"] = *req.AssignedUserID
	}
	actor := userID
	_ = h.state.Store.InsertTicketAudit(&models.TicketAuditEvent{
		TicketID:    id,
		EventType:   eventType,
		ActorUserID: &actor,
		Metadata:    meta,
	})

	// Notify the new assignee (if any and not self-assign).
	if req.AssignedUserID != nil && *req.AssignedUserID != userID {
		link := "/tickets/" + strconv.Itoa(id)
		EmitTicketNotification(h.state, []string{*req.AssignedUserID},
			NotifyTypeTicketAssigned,
			"Ticket #"+strconv.Itoa(id)+" assigned to you",
			t.Title, link)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

type watcherRequest struct {
	UserID   string `json:"userId"`
	Username string `json:"username,omitempty"`
	CanReply *bool  `json:"canReply,omitempty"`
}

// AddWatcher POST /api/tickets/{id}/watchers
// Users may add watchers when tickets.allow_users_to_add_watchers=TRUE.
func (h *TicketsHandler) AddWatcher(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	perms := LoadEffectivePermissions(h.state, userID)
	settings := LoadTicketSettings(h.state)
	id, _ := strconv.Atoi(mux.Vars(r)["id"])
	t, err := h.state.Store.GetTicket(id)
	if err != nil || t == nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	if !perms.IsAdmin && !perms.IsSupport {
		if t.UserID != userID || !settings.AllowUsersToAddWatchers {
			sendJSONError(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
	var req watcherRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	// Resolve by username if userId omitted — simplifies the UI.
	target := req.UserID
	if target == "" && req.Username != "" {
		u, err := h.state.Store.GetUserByUsername(strings.TrimSpace(req.Username))
		if err != nil || u == nil {
			sendJSONError(w, "User not found", http.StatusNotFound)
			return
		}
		target = u.ID
	}
	if target == "" {
		sendJSONError(w, "userId or username required", http.StatusBadRequest)
		return
	}
	// The username branch above proves the user exists; a supplied userId did
	// not, so a wrong id reached the insert and came back as a foreign-key
	// violation dressed up as "Failed to add watcher" with a 500. Same answer
	// for both branches.
	if u, uerr := h.state.Store.GetUserByID(target); uerr != nil || u == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	canReplyFlag := settings.WatchersDefaultCanReply
	if req.CanReply != nil {
		canReplyFlag = *req.CanReply
	}
	// Non-support users can never grant reply rights — admin policy only.
	if !perms.IsAdmin && !perms.IsSupport {
		canReplyFlag = settings.WatchersDefaultCanReply
	}
	uid := userID
	if err := h.state.Store.AddTicketWatcher(&models.TicketWatcher{
		TicketID: id,
		UserID:   target,
		CanReply: canReplyFlag,
		AddedBy:  &uid,
	}); err != nil {
		sendJSONError(w, "Failed to add watcher", http.StatusInternalServerError)
		return
	}
	actor := userID
	_ = h.state.Store.InsertTicketAudit(&models.TicketAuditEvent{
		TicketID:    id,
		EventType:   TicketEventWatcherAdded,
		ActorUserID: &actor,
		Metadata: map[string]interface{}{
			"target_user_id": target,
			"can_reply":      canReplyFlag,
		},
	})

	// Notify the user being CC'd, unless they added themselves.
	if target != userID {
		link := "/tickets/" + strconv.Itoa(id)
		EmitTicketNotification(h.state, []string{target},
			NotifyTypeTicketWatcherAdd,
			"Added to ticket #"+strconv.Itoa(id),
			t.Title, link)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// RemoveWatcher DELETE /api/tickets/{id}/watchers/{userId} - removes a watcher
// and records it in the audit trail.
func (h *TicketsHandler) RemoveWatcher(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	perms := LoadEffectivePermissions(h.state, userID)
	id, _ := strconv.Atoi(mux.Vars(r)["id"])
	targetID, ok := parseUserID(w, r, "userId")
	if !ok {
		return
	}
	t, err := h.state.Store.GetTicket(id)
	if err != nil || t == nil {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	// Ticket owner can remove watchers; the watcher themselves can remove
	// themselves; support+admin can do either.
	//
	// The targetID == userID arm looks like it lets a stranger act on any
	// ticket, and on its own it does - but the DELETE below is scoped to
	// (ticket, target), so all it can ever reach is the caller's OWN watcher
	// row, which they are entitled to remove. What made that arm a hole was the
	// code after it, not the arm itself.
	if !(perms.IsAdmin || perms.IsSupport || t.UserID == userID || targetID == userID) {
		sendJSONError(w, "Forbidden", http.StatusForbidden)
		return
	}
	removed, err := h.state.Store.RemoveTicketWatcher(id, targetID)
	if err != nil {
		sendJSONError(w, "Remove failed", http.StatusInternalServerError)
		return
	}
	// A delete that matched nothing is not a removal, and saying otherwise had
	// two costs. Any authenticated account could POST its own id against any
	// ticket number and have an audit event written into a ticket it has
	// nothing to do with - the log of who touched a support ticket was writable
	// by strangers. And the 200/404 split answered "does ticket N exist" for
	// anyone willing to count upwards. Both end here: nothing removed reads
	// exactly like a ticket that is not there.
	if !removed {
		sendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	actor := userID
	_ = h.state.Store.InsertTicketAudit(&models.TicketAuditEvent{
		TicketID:    id,
		EventType:   TicketEventWatcherRemoved,
		ActorUserID: &actor,
		Metadata: map[string]interface{}{
			"target_user_id": targetID,
		},
	})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ListMyServersViaTickets GET /api/me/servers/via-tickets
// Drives the sidebar tab. Empty for non-support users.
func (h *TicketsHandler) ListMyServersViaTickets(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	perms := LoadEffectivePermissions(h.state, userID)
	if !perms.IsAdmin && !perms.IsSupport {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"servers": []models.Server{},
		})
		return
	}
	servers, err := h.state.Store.ListServersViaActiveTickets(userID)
	if err != nil {
		sendJSONError(w, "Failed to load", http.StatusInternalServerError)
		return
	}
	if servers == nil {
		servers = []models.Server{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"servers": servers,
	})
}

// ── tiny helpers ─────────────────────────────────────────────────────

func validTicketStatus(s string) bool {
	return s == "open" || s == "in_progress" || s == "waiting_user" || s == "resolved" || s == "closed"
}

func parseCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseIntDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
