package authz

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
)

// The server audit trail is the OWNER's accountability record over the people
// they invited. Until this file existed it was written by the handlers that
// remembered to write it, and measured on production that was four of them:
// power, rename, setup and the invite itself. A delegate holding the "Server
// admin" preset created a file, overwrote it, DELETED it, ran a console
// command and created a backup job, and the owner's trail showed none of it.
//
// Asking every handler to remember is the same shape as the bug: a guard N
// siblings take and the N+1th does not. So the record is written where every
// server-scoped action already passes and both the capability and the server
// are already known - RequireCap - and a handler that wants to say more writes
// its own richer row instead, which MarkAudited then keeps from being
// duplicated here.

// WriteAuditFunc records one authorized server-scoped write. RequireCap calls
// it only for an action that SUCCEEDED and that no handler has already
// described itself (see MarkAudited), so a recorder implements neither check.
// status is the response code the handler produced.
type WriteAuditFunc func(r *http.Request, serverID int, capID string, status int)

// SetServerWriteAudit installs the recorder. Unset means nothing is recorded,
// which is what the unit tests and any embedder without a store get.
func (r *Resolver) SetServerWriteAudit(fn WriteAuditFunc) { r.writeAudit = fn }

type auditSlotKey struct{}

// withAuditSlot puts a "a handler already recorded this" flag in the context.
func withAuditSlot(ctx context.Context) context.Context {
	done := false
	return context.WithValue(ctx, auditSlotKey{}, &done)
}

// MarkAudited records that a handler has written its own audit row for this
// request, so the generic recorder stays quiet rather than logging the same
// action twice under two names. Safe on a request that has no slot.
func MarkAudited(ctx context.Context) {
	if p, ok := ctx.Value(auditSlotKey{}).(*bool); ok && p != nil {
		*p = true
	}
}

// WasAudited reports what MarkAudited recorded.
func WasAudited(ctx context.Context) bool {
	p, ok := ctx.Value(auditSlotKey{}).(*bool)
	return ok && p != nil && *p
}

// auditedWriter remembers the status code so only a request that SUCCEEDED is
// recorded. A row for an action that was refused downstream would describe
// something that never happened, which is worse in an accountability log than
// a missing row.
type auditedWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *auditedWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditedWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *auditedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards rather than claiming the capability unconditionally: a
// wrapper that says it can hijack when the writer underneath cannot turns a
// working upgrade into a panic.
func (w *auditedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the underlying ResponseWriter is not a Hijacker")
	}
	return h.Hijack()
}

// writeSlot carries what a handler-side capability check authorized, from the
// handler back out to the wrapper that knows how the request ended.
type writeSlot struct {
	serverID int
	capID    string
}

type writeSlotKey struct{}

// StashServerWrite is how a route that checks its capability in the HANDLER
// tells AuditResolvedWrite what it just authorized. A no-op without the
// wrapper, so handlers called directly from a unit test are unaffected.
func StashServerWrite(ctx context.Context, serverID int, capID string) {
	if p, ok := ctx.Value(writeSlotKey{}).(*writeSlot); ok && p != nil {
		p.serverID, p.capID = serverID, capID
	}
}

// AuditResolvedWrite records a successful action on a route whose capability
// check lives in the handler rather than in RequireCap.
//
// The file manager is why it exists. Its ten routes carry the server in a
// ?server_uuid= query parameter rather than a path variable, so they resolve
// their own capability and are exempt from the chokepoint - which is how
// DELETING A FILE came to be the one destructive action the owner's audit
// trail missed entirely. The server id comes back from the handler's own
// resolution rather than being parsed again out here, so this never has to
// touch the request body: re-reading a multipart upload to find out which
// server it was for would break the upload.
func (r *Resolver) AuditResolvedWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r.writeAudit == nil {
			next(w, req)
			return
		}
		slot := &writeSlot{}
		req = req.WithContext(context.WithValue(withAuditSlot(req.Context()), writeSlotKey{}, slot))
		aw := &auditedWriter{ResponseWriter: w}
		next(aw, req)
		if slot.serverID == 0 || aw.status < 200 || aw.status >= 300 || WasAudited(req.Context()) {
			return
		}
		// Reads pass through the same resolution, and browsing a directory is
		// not an action done TO the server.
		if c, ok := Get(slot.capID); !ok || c.Verb == VerbRead {
			return
		}
		r.writeAudit(req, slot.serverID, slot.capID, aw.status)
	}
}
