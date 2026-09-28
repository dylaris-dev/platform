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

// AccessEpochTTL is deliberately just past the ticket lifetime. A ticket older
// than its own expiry is refused for being expired, so a stamp older than that
// answers nothing and would only grow Redis.
const AccessEpochTTL = BeamTicketTTL + 5*time.Minute

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
	return c.IssuedAt.Unix() < stamp, nil
}
