package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/pkg/leader"
)

// nodeLinkAnswer is what the Hub publishes under hub:node-link:<node id>: the
// ONE link that serves a node.
//
// CROSS-REPO WIRE CONTRACT, produced by gateway/hub/pkg/hub/node_link.go. Both
// repos compile their own copy of this shape, so a field may be ADDED but never
// renamed or repurposed. V tells an incompatible future shape apart instead of
// half-reading it.
type nodeLinkAnswer struct {
	V      int    `json:"v"`
	LinkID uint   `json:"link_id"`
	Token  string `json:"token"`
	Kind   string `json:"kind"`
}

const (
	// nodeLinkAnswerVersion is the only shape this reader understands.
	nodeLinkAnswerVersion = 1

	// nodeLinkKeyPrefix is followed by the node's server-assigned identity
	// (nodes.token), the same value DeriveLinkToken is given.
	nodeLinkKeyPrefix = "hub:node-link:"

	// nodeLinkInterval matches the Hub's publish cadence. A faster loop would
	// only read the same answer twice.
	nodeLinkInterval = 60 * time.Second
)

// effectiveLinkToken is the token of the link a node's ROUTES belong to: the one
// the Hub named for it, else the derived token of the node-managed Link. Every
// route write goes through here, because the Hub binds a route by looking its
// link up BY TOKEN and silently drops one whose token matches no link - which is
// what the derived token is for a node served by a self-enrolled Link.
//
// Only routes follow it. The node-managed Link's own identity (its ACL, the
// AuthResult link secret, the deploy bundle) keeps deriving: that Link runs with
// the derived token whichever link the routes point at.
func effectiveLinkToken(n *models.Node, clusterSecret string) string {
	if n.LinkToken != "" {
		return n.LinkToken
	}
	return DeriveLinkToken(n.Token, clusterSecret)
}

// nodeLinkStore is the narrow store surface the learner needs. Nodes are LISTED,
// so a failed read stops the pass instead of reading as "no such node".
type nodeLinkStore interface {
	ListNodes() ([]models.Node, error)
	ListServersByNode(nodeID int) ([]models.Server, error)
	SetNodeLinkToken(id int, token string) error
}

// NodeLinkLearner keeps nodes.link_token in step with the link the Hub says
// serves each node, and moves a node's routes when that link changes.
//
// Core used to DERIVE the link token for every route. A Link that enrols itself
// at the Hub gets a generated token, so to routing it is a different link, and
// the Hub dropped every route Core sent for that node without a word. The Hub
// knows which link serves a node; Core now asks it instead of guessing.
//
// A switch is debounced. The Hub judges liveness once per pass from a 15s key,
// so during a cutover its answer can name, for up to a minute, a link that is
// enrolled but not connected yet. Routes move only when the new link is online
// at the moment of the decision AND the Hub named that same link on the previous
// pass too. The "named last pass" memory is in-process and leader-scoped: a
// leader change restarts the count, which costs a minute and nothing else.
//
// Absence of an answer never clears the column. The key has a TTL, and a Hub
// that is restarting says nothing; that is not "this node has no link".
//
// RunOnce is not safe for concurrent use; the ticker is its only caller.
type NodeLinkLearner struct {
	store  nodeLinkStore
	gw     *RedisGateway
	leader leader.Election
	// seen is the token the Hub named on the previous pass for each node whose
	// routes point elsewhere. Rebuilt every pass, so a pass that does not name
	// it breaks the chain.
	seen map[string]string
	// justSwitched is the token a node's routes were moved to on the PREVIOUS
	// pass, one entry per node whose switch just completed. It triggers exactly
	// one repeat push (see the comment above the push loop in apply) and is
	// rebuilt every pass like seen, so it never survives a second pass.
	justSwitched map[string]string
}

func NewNodeLinkLearner(s nodeLinkStore, g *RedisGateway) *NodeLinkLearner {
	return &NodeLinkLearner{store: s, gw: g}
}

func (l *NodeLinkLearner) SetLeader(e leader.Election) { l.leader = e }

func (l *NodeLinkLearner) Start(ctx context.Context) {
	log.Printf("Node link learner started (interval: %s)", nodeLinkInterval)
	l.RunOnce(ctx)
	ticker := time.NewTicker(nodeLinkInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.RunOnce(ctx)
			}
		}
	}()
}

// RunOnce reads every answer the Hub published and applies it.
func (l *NodeLinkLearner) RunOnce(ctx context.Context) {
	// Taken before any early return, so a pass that is skipped or fails also
	// breaks the "named twice in a row" chain rather than bridging it, and
	// drops a pending repeat-push marker the same way - a leader change simply
	// loses it.
	prev := l.seen
	l.seen = map[string]string{}
	prevSwitched := l.justSwitched
	l.justSwitched = map[string]string{}

	if l.leader != nil && !l.leader.IsLeader() {
		return
	}
	if l.store == nil || l.gw == nil || l.gw.redis == nil {
		return
	}
	answers, err := l.answers(ctx)
	if err != nil {
		logErrf("node-link", "%v", err)
		return
	}
	if len(answers) == 0 {
		return
	}
	nodes, err := l.store.ListNodes()
	if err != nil {
		logErrf("node-link", "list nodes: %v", err)
		return
	}
	for _, n := range nodes {
		// A node with no answer keeps what it has; an answer for a node Core
		// does not know is never looked at.
		if a, ok := answers[n.Token]; ok {
			l.apply(ctx, n, a, prev[n.Token], prevSwitched[n.Token])
		}
	}
}

// answers collects every well-formed answer, keyed by node identity taken from
// the KEY: the payload does not carry one.
func (l *NodeLinkLearner) answers(ctx context.Context) (map[string]nodeLinkAnswer, error) {
	rdb := l.gw.redis
	out := map[string]nodeLinkAnswer{}
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, nodeLinkKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan %s*: %w", nodeLinkKeyPrefix, err)
		}
		for _, key := range keys {
			nodeID := strings.TrimPrefix(key, nodeLinkKeyPrefix)
			if nodeID == "" {
				continue
			}
			val, err := rdb.Get(ctx, key).Result()
			if err != nil {
				// Expired between SCAN and GET is the normal race. Anything else
				// is Redis failing, and must not read as "no answers".
				if errors.Is(err, redis.Nil) {
					continue
				}
				return nil, fmt.Errorf("read the answer for node %s: %w", tokenPrefix(nodeID), err)
			}
			var a nodeLinkAnswer
			if json.Unmarshal([]byte(val), &a) != nil || a.V != nodeLinkAnswerVersion || a.Token == "" {
				continue
			}
			out[nodeID] = a
		}
		if next == 0 {
			return out, nil
		}
		cursor = next
	}
}

// apply reconciles one node against the Hub's answer. seenBefore is what the
// Hub named for this node on the previous pass, if its routes pointed
// elsewhere. justSwitchedTo is the token this node's routes were moved to on
// the previous pass, if a switch completed then - it triggers one repeat push,
// see the comment above the push loop below.
func (l *NodeLinkLearner) apply(ctx context.Context, n models.Node, a nodeLinkAnswer, seenBefore, justSwitchedTo string) {
	current := effectiveLinkToken(&n, l.gw.clusterSecret)
	if a.Token == current {
		// The routes already point here - every node whose answer is its own
		// derived token, and the node this apply just switched last pass. Only
		// the column is filled if it does not already agree.
		if n.LinkToken != a.Token {
			if err := l.store.SetNodeLinkToken(n.ID, a.Token); err != nil {
				logErrf("node-link", "node %d (%s): store link: %v", n.ID, tokenPrefix(n.Token), err)
			}
		}
		if justSwitchedTo == a.Token {
			l.repush(n, a.Token)
		}
		return
	}

	l.seen[n.Token] = a.Token
	if seenBefore != a.Token {
		log.Printf("[node-link] node %d (%s): the hub names %s link %d (%s) instead of %s; routes stay until it is online and named again",
			n.ID, tokenPrefix(n.Token), a.Kind, a.LinkID, tokenPrefix(a.Token), tokenPrefix(current))
		return
	}
	online, err := l.gw.redis.Exists(ctx, "online_link:"+a.Token).Result()
	if err != nil {
		logErrf("node-link", "node %d (%s): liveness of link %s: %v", n.ID, tokenPrefix(n.Token), tokenPrefix(a.Token), err)
		return
	}
	if online == 0 {
		return // said once, when first named
	}

	servers, err := l.store.ListServersByNode(n.ID)
	if err != nil {
		logErrf("node-link", "node %d (%s): list servers: %v", n.ID, tokenPrefix(n.Token), err)
		return
	}
	// Routes first, column second. Stored first, a failed push would leave the
	// column agreeing with the Hub while routes still point at the old link, and
	// no later pass would see a difference to act on. This way a failure retries
	// the whole node next pass; migrate_routes is idempotent.
	//
	// A route CreateServerRoute creates in the window between it reading the old
	// link_token and SetNodeLinkToken landing below can reach the Hub with the
	// old token AFTER this pass's migrate_routes, and stay bound to a link that
	// may be gone: the column already agrees by then, so no later pass sees a
	// difference to act on. Fixed by the justSwitchedTo check at the top of this
	// function: the pass right after a switch re-pushes migrate_routes once more
	// for the same node, which catches that straggler the same way the retry
	// above catches a mid-push failure.
	//
	// What the repeat push cannot repair: the learner lists server S on node X,
	// the migration orchestrator moves S to node Y and queues migrate(S, Y), then
	// the learner queues migrate(S, X-new) for the link switch it is
	// reconciling here. The Hub applies both in the order it receives them, so
	// S's routes can end up on X's link while S actually runs on Y. That needs a
	// node move and a link switch to land in the same moment; accepted
	// knowingly, not fixed here.
	for _, s := range servers {
		if err := l.gw.pushToQueue(hubQueueMessage{
			Action:       "migrate_routes",
			ServerUUID:   s.UUID,
			NewLinkToken: a.Token,
		}); err != nil {
			logErrf("node-link", "node %d (%s): migrate routes of %s: %v", n.ID, tokenPrefix(n.Token), s.UUID, err)
			return
		}
	}
	if err := l.store.SetNodeLinkToken(n.ID, a.Token); err != nil {
		logErrf("node-link", "node %d (%s): store link: %v", n.ID, tokenPrefix(n.Token), err)
		return
	}
	l.justSwitched[n.Token] = a.Token
	log.Printf("[node-link] node %d (%s): routes moved from link %s to %s link %d (%s), %d server(s)",
		n.ID, tokenPrefix(n.Token), tokenPrefix(current), a.Kind, a.LinkID, tokenPrefix(a.Token), len(servers))
}

// repush re-sends migrate_routes for every server on n, once, on the pass
// right after n's routes were moved to token. It catches a route
// CreateServerRoute created in the race described above the push loop in
// apply. Best-effort: a failure here is logged and dropped rather than
// retried, same as the routine push it repeats - there is no marker left to
// retry from once this pass is done, and the case it exists for is rare
// enough that the next genuine switch is the next chance.
func (l *NodeLinkLearner) repush(n models.Node, token string) {
	servers, err := l.store.ListServersByNode(n.ID)
	if err != nil {
		logErrf("node-link", "node %d (%s): re-push list servers: %v", n.ID, tokenPrefix(n.Token), err)
		return
	}
	for _, s := range servers {
		if err := l.gw.pushToQueue(hubQueueMessage{
			Action:       "migrate_routes",
			ServerUUID:   s.UUID,
			NewLinkToken: token,
		}); err != nil {
			logErrf("node-link", "node %d (%s): re-push migrate routes of %s: %v", n.ID, tokenPrefix(n.Token), s.UUID, err)
			return
		}
	}
	log.Printf("[node-link] node %d (%s): re-pushed migrate_routes to %s link (%d server(s)), the pass after the switch",
		n.ID, tokenPrefix(n.Token), tokenPrefix(token), len(servers))
}
