package auth

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// A beam ticket is a bearer token with a 30 minute life and nothing re-reads
// it: the relay checks the signature, the node checks the proof, and neither
// asks Core anything. So taking somebody's access away stopped NEW tickets and
// left an outstanding one working to its expiry - the same for a suspended
// account, whose minting is refused while a ticket it already holds is not.
//
// The access epoch closes the half that can be closed cheaply. Core stamps the
// SERVER whenever something changes about who may reach its files, and the node
// refuses, at Authenticate, any ticket minted before that stamp. An open
// session is not torn down; what stops is opening a new one on an old ticket.
//
// Keyed by server UUID rather than by user because the UUID is immutable and is
// in the ticket, while the username in there can be renamed - and because "the
// access rules for this server changed, re-mint" is the honest statement. A
// suspension stamps every server the account owns.
//
// Both sides spell the key through this file so the two cannot drift.

const accessEpochPrefix = "beam:access-epoch:"

// AccessEpochTTL outlives every session a stamp has to end, not just the
// ticket. It used to be just past the ticket's 30 minutes, which was enough
// while only Authenticate read it; open sessions re-check it now (the node's
// sessionLive), and a stamp that expired while a session sat idle answered
// "nothing changed" and gave the revoked session its rights back. Sessions are
// capped at a day on the node; a stamp is a few bytes per server.
const AccessEpochTTL = 30 * 24 * time.Hour

// AccessEpochKey is the stamp for one server. Exported because the Redis ACL
// rules are built from the same prefix.
func AccessEpochKey(serverUUID string) string { return accessEpochPrefix + serverUUID }

// AccessEpochPrefix is what the node is granted read on.
func AccessEpochPrefix() string { return accessEpochPrefix }

// BumpAccessEpoch records that who may reach this server's files has changed.
//
// Called by CORE. A failure is worth logging but not worth failing the
// permission change over: the change itself is already in the database, and
// refusing it here would leave the caller thinking it did not happen.
func BumpAccessEpoch(ctx context.Context, rdb *redis.Client, serverUUID string) error {
	if rdb == nil || serverUUID == "" {
		return nil
	}
	return rdb.Set(ctx, AccessEpochKey(serverUUID), time.Now().Unix(), AccessEpochTTL).Err()
}

// TicketPredatesAccessChange reports whether this ticket was minted before the
// last access change on its server, and is therefore stale.
//
// Called by the NODE. It fails OPEN, like every other Redis read on this path:
// a Redis outage must not stop a paying customer reaching their own files, and
// the window it reopens is the 30 minutes that existed before this file did.
// The error is returned so the caller can say so in a log rather than silently
// treating "could not ask" as "fine".
func TicketPredatesAccessChange(ctx context.Context, rdb *redis.Client, c *BeamClaims) (bool, error) {
	if rdb == nil || c == nil || c.ServerUUID == "" {
		return false, nil
	}
	// IssuedAt is what the stamp is compared against, and on the per-node proof
	// path nothing checks the signature - so it is covered by proofPayload for
	// exactly this reason. A ticket with no iat cannot be judged; refusing it
	// would break every ticket minted by a Core that predates this.
	if c.IssuedAt == nil {
		return false, nil
	}
	stamp, err := rdb.Get(ctx, AccessEpochKey(c.ServerUUID)).Int64()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// <= : the claim and the stamp are whole seconds, and a ticket minted in
	// the same second as the change cannot be told apart from one minted just
	// before it. Refusing it costs the holder a reconnect a second later.
	return c.IssuedAt.Unix() <= stamp, nil
}
