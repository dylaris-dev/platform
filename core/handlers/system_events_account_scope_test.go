package handlers

import "testing"

// A frame naming an ACCOUNT goes to that account and to admins, and to nobody
// else.
//
// packs.changed carried the pack owner's id and users.changed the subject's, on
// a fan-out that reached every authenticated session, while the scope filter
// looked only at serverId. The owner id is the expensive half: it is the first
// segment of modpacks/<ownerID>/<slug>/<version>/pack.mrpack, and the pack
// mirror serves that path to anyone without a credential by design. The layout is
// accepted because the path cannot be derived from outside - which stops being
// true the moment one of its unknown segments is broadcast.
func TestSystemEventsAccountScope(t *testing.T) {
	h := &SystemEventsHandler{}

	tests := []struct {
		name    string
		payload string
		userID  string
		isAdmin bool
		want    bool
		why     string
	}{
		{
			name:    "another owner's pack change",
			payload: `{"type":"packs.changed","payload":{"ownerId":"owner-1"}}`,
			userID:  "someone-else",
			want:    false,
			why:     "hands a stranger the first path segment of that owner's private .mrpack URLs",
		},
		{
			name:    "your own pack change",
			payload: `{"type":"packs.changed","payload":{"ownerId":"owner-1"}}`,
			userID:  "owner-1",
			want:    true,
			why:     "the pack list would stop refreshing after your own edit",
		},
		{
			name:    "an admin sees everything",
			payload: `{"type":"packs.changed","payload":{"ownerId":"owner-1"}}`,
			userID:  "root",
			isAdmin: true,
			want:    true,
			why:     "admin screens list every owner's packs",
		},
		{
			name:    "a user change about someone else",
			payload: `{"type":"users.changed","payload":{"userId":"user-9"}}`,
			userID:  "someone-else",
			want:    false,
			why:     "discloses who else exists and when their account changed",
		},
		{
			name:    "the grant that changes YOUR entitlement",
			payload: `{"type":"users.changed","payload":{"userId":"user-9"}}`,
			userID:  "user-9",
			want:    true,
			why:     "the panel refreshes its entitlement on this frame; dropping it strands a tenant on a stale plan",
		},
		{
			name:    "a payload naming no account at all",
			payload: `{"type":"settings.changed","payload":{}}`,
			userID:  "anyone",
			want:    true,
			why:     "most frames are empty cache-invalidation signals and must still fan out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.mayReceive(eventScopeRequest(tt.userID, tt.isAdmin), tt.payload)
			if got != tt.want {
				t.Errorf("mayReceive = %v, want %v: %s", got, tt.want, tt.why)
			}
		})
	}
}

// The ids are strings in the payload because they are strings in the model. A
// decoder that silently fails to read them returns "" and every frame goes out
// unscoped, with nothing failing anywhere - so the shape is pinned here rather
// than trusted.
func TestEventAccountIDReadsBothSpellings(t *testing.T) {
	if got := eventAccountID(`{"payload":{"ownerId":"o-1"}}`); got != "o-1" {
		t.Errorf("ownerId: got %q", got)
	}
	if got := eventAccountID(`{"payload":{"userId":"u-1"}}`); got != "u-1" {
		t.Errorf("userId: got %q", got)
	}
	if got := eventAccountID(`{"payload":{"serverId":7}}`); got != "" {
		t.Errorf("a server-scoped frame must not read as account-scoped: got %q", got)
	}
	if got := eventAccountID(`not json`); got != "" {
		t.Errorf("unparseable payload: got %q", got)
	}
}
