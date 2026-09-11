package services

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
)

const nodeLinkTestSecret = "test-cluster-secret"

// nodeLinkFakeStore is the learner's narrow store surface, backed by a slice.
type nodeLinkFakeStore struct {
	nodes   []models.Node
	servers map[int][]models.Server
	// writes counts column writes, so a test can prove an answer that agrees
	// with the row writes NOTHING rather than rewriting it every minute.
	writes int
}

func (f *nodeLinkFakeStore) ListNodes() ([]models.Node, error) {
	return append([]models.Node(nil), f.nodes...), nil
}

func (f *nodeLinkFakeStore) ListServersByNode(nodeID int) ([]models.Server, error) {
	return f.servers[nodeID], nil
}

func (f *nodeLinkFakeStore) SetNodeLinkToken(id int, token string) error {
	f.writes++
	for i := range f.nodes {
		if f.nodes[i].ID == id {
			f.nodes[i].LinkToken = token
		}
	}
	return nil
}

func (f *nodeLinkFakeStore) linkToken(id int) string {
	for _, n := range f.nodes {
		if n.ID == id {
			return n.LinkToken
		}
	}
	return ""
}

type switchableLeader struct{ leading bool }

func (s *switchableLeader) IsLeader() bool { return s.leading }

// One node (id 1, identity "node-a") with two servers.
func nodeLinkFixture(t *testing.T) (*NodeLinkLearner, *nodeLinkFakeStore, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	st := &nodeLinkFakeStore{
		nodes: []models.Node{{ID: 1, Token: "node-a"}},
		servers: map[int][]models.Server{
			1: {{ID: 10, UUID: "srv-10", NodeID: 1}, {ID: 11, UUID: "srv-11", NodeID: 1}},
		},
	}
	return NewNodeLinkLearner(st, NewRedisGateway(rdb, nil, nodeLinkTestSecret)), st, mr, rdb
}

func hubNamesLink(mr *miniredis.Miniredis, nodeID, token string) {
	mr.Set("hub:node-link:"+nodeID, fmt.Sprintf(`{"v":1,"link_id":7,"token":%q,"kind":"enrolled"}`, token))
}

func passes(l *NodeLinkLearner, n int) {
	for i := 0; i < n; i++ {
		l.RunOnce(context.Background())
	}
}

// Every node today: the Hub's answer IS the derived token. The column gets
// filled and not one route may move.
func TestNodeLink_AnswerIsTheDerivedToken_FillsColumnMovesNothing(t *testing.T) {
	l, st, mr, rdb := nodeLinkFixture(t)
	derived := DeriveLinkToken("node-a", nodeLinkTestSecret)
	hubNamesLink(mr, "node-a", derived)
	mr.Set("online_link:"+derived, "1")

	passes(l, 3)

	if got := st.linkToken(1); got != derived {
		t.Errorf("link_token = %q, want the derived token stored", got)
	}
	if st.writes != 1 {
		t.Errorf("writes = %d, want exactly 1 (filled once, then already current)", st.writes)
	}
	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 0 {
		t.Errorf("queued %+v, want nothing: the routes already point at this link", msgs)
	}
}

func TestNodeLink_AnswerIsTheStoredToken_WritesNothing(t *testing.T) {
	l, st, mr, rdb := nodeLinkFixture(t)
	st.nodes[0].LinkToken = "generated"
	hubNamesLink(mr, "node-a", "generated")
	mr.Set("online_link:generated", "1")

	passes(l, 2)

	if st.writes != 0 {
		t.Errorf("writes = %d, want 0", st.writes)
	}
	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 0 {
		t.Errorf("queued %+v, want nothing", msgs)
	}
}

// The case this exists for: a self-enrolled Link with a generated token takes
// over the node. Once it is online and named twice, every server's routes move
// to it and the column records it.
func TestNodeLink_NewLink_OnlineAndNamedTwice_MovesEveryServer(t *testing.T) {
	l, st, mr, rdb := nodeLinkFixture(t)
	hubNamesLink(mr, "node-a", "generated")
	mr.Set("online_link:generated", "1")

	l.RunOnce(context.Background())
	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 0 || st.writes != 0 {
		t.Fatalf("after ONE naming: queued %+v, writes %d; want nothing yet", msgs, st.writes)
	}

	l.RunOnce(context.Background())
	msgs := readHubQueueMessages(t, rdb)
	if len(msgs) != 2 {
		t.Fatalf("queued %d messages, want one per server (2): %+v", len(msgs), msgs)
	}
	for i, want := range []string{"srv-10", "srv-11"} {
		m := msgs[i]
		if m.Action != "migrate_routes" || m.ServerUUID != want || m.NewLinkToken != "generated" {
			t.Errorf("message %d = %+v, want migrate_routes %s -> generated", i, m, want)
		}
	}
	if got := st.linkToken(1); got != "generated" {
		t.Errorf("link_token = %q, want generated", got)
	}

	// The pass right after the switch re-pushes once more, to catch a route
	// created in the race window around it - see TestNodeLink_SwitchRepushesOnceOnTheNextPass.
	l.RunOnce(context.Background())
	if got := len(readHubQueueMessages(t, rdb)); got != 4 {
		t.Errorf("queued %d messages after the repeat pass, want 4 (2 + 2 repeated)", got)
	}

	l.RunOnce(context.Background())
	if got := len(readHubQueueMessages(t, rdb)); got != 4 {
		t.Errorf("a settled node queued more: %d messages in total, want still 4", got)
	}
}

// Condition (a): a link the Hub names but that is not connected reaches no
// player. Named for as long as you like, nothing moves until it is online.
func TestNodeLink_NewLink_NotOnline_Waits(t *testing.T) {
	l, st, mr, rdb := nodeLinkFixture(t)
	hubNamesLink(mr, "node-a", "generated")

	passes(l, 4)
	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 0 {
		t.Fatalf("moved routes to an offline link: %+v", msgs)
	}
	if st.writes != 0 || st.linkToken(1) != "" {
		t.Fatalf("stored %q (%d writes) for a switch that did not happen", st.linkToken(1), st.writes)
	}

	mr.Set("online_link:generated", "1")
	l.RunOnce(context.Background())
	if got := len(readHubQueueMessages(t, rdb)); got != 2 {
		t.Errorf("queued %d once online, want 2", got)
	}
}

// Condition (b): named in two CONSECUTIVE passes. An answer that flips, or
// goes missing for a pass, restarts the count.
func TestNodeLink_NewLink_MustBeNamedTwiceInARow(t *testing.T) {
	l, st, mr, rdb := nodeLinkFixture(t)
	mr.Set("online_link:generated", "1")
	mr.Set("online_link:other", "1")

	hubNamesLink(mr, "node-a", "generated")
	l.RunOnce(context.Background())
	mr.Del("hub:node-link:node-a")
	l.RunOnce(context.Background()) // the chain breaks here
	hubNamesLink(mr, "node-a", "generated")
	l.RunOnce(context.Background())
	hubNamesLink(mr, "node-a", "other")
	l.RunOnce(context.Background()) // and here
	hubNamesLink(mr, "node-a", "generated")
	l.RunOnce(context.Background())

	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 0 || st.writes != 0 {
		t.Fatalf("moved on an answer never named twice in a row: queued %+v, writes %d", msgs, st.writes)
	}

	l.RunOnce(context.Background())
	if got := len(readHubQueueMessages(t, rdb)); got != 2 {
		t.Errorf("queued %d after the second consecutive naming, want 2", got)
	}
}

// A replica that is not leading does nothing, and losing the lease restarts
// the count: a naming before the gap must not pair with one after it.
func TestNodeLink_LeadershipGapRestartsTheCount(t *testing.T) {
	l, st, mr, rdb := nodeLinkFixture(t)
	lead := &switchableLeader{leading: true}
	l.SetLeader(lead)
	hubNamesLink(mr, "node-a", "generated")
	mr.Set("online_link:generated", "1")

	l.RunOnce(context.Background())
	lead.leading = false
	passes(l, 2)
	lead.leading = true
	l.RunOnce(context.Background())

	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 0 || st.writes != 0 {
		t.Fatalf("queued %+v, writes %d; want nothing across a leadership gap", msgs, st.writes)
	}
	l.RunOnce(context.Background())
	if got := len(readHubQueueMessages(t, rdb)); got != 2 {
		t.Errorf("queued %d, want 2 after two consecutive passes as leader", got)
	}
}

// Wrong version, no token, not JSON at all, and a node Core does not know:
// none of them may touch a row or a route.
func TestNodeLink_IgnoresWhatItCannotRead(t *testing.T) {
	l, st, mr, rdb := nodeLinkFixture(t)
	st.nodes = append(st.nodes,
		models.Node{ID: 2, Token: "node-b"},
		models.Node{ID: 3, Token: "node-c"})
	mr.Set("hub:node-link:node-a", `{"v":2,"link_id":7,"token":"generated","kind":"enrolled"}`)
	mr.Set("hub:node-link:node-b", `{"v":1,"link_id":7,"token":"","kind":"enrolled"}`)
	mr.Set("hub:node-link:node-c", `not json`)
	hubNamesLink(mr, "node-unknown", "generated")
	mr.Set("online_link:generated", "1")

	passes(l, 3)

	if st.writes != 0 {
		t.Errorf("writes = %d, want 0", st.writes)
	}
	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 0 {
		t.Errorf("queued %+v, want nothing", msgs)
	}
}

// Absence is not "forget this link": the key has a TTL and a restarting Hub
// says nothing for a while.
func TestNodeLink_AbsentAnswerKeepsTheColumn(t *testing.T) {
	l, st, _, rdb := nodeLinkFixture(t)
	st.nodes[0].LinkToken = "generated"

	passes(l, 2)

	if got := st.linkToken(1); got != "generated" || st.writes != 0 {
		t.Errorf("link_token = %q after %d writes, want generated untouched", got, st.writes)
	}
	if msgs := readHubQueueMessages(t, rdb); len(msgs) != 0 {
		t.Errorf("queued %+v, want nothing", msgs)
	}
}

// A route CreateServerRoute creates in the race window around a switch can
// reach the Hub with the OLD token after the switch pass's migrate_routes, and
// the column already agreeing means no later pass would normally see a
// difference to act on. The repair: the pass right after a switch re-pushes
// migrate_routes once more for every server on that node, then stops.
func TestNodeLink_SwitchRepushesOnceOnTheNextPass(t *testing.T) {
	l, st, mr, rdb := nodeLinkFixture(t)
	hubNamesLink(mr, "node-a", "generated")
	mr.Set("online_link:generated", "1")

	l.RunOnce(context.Background()) // named once - nothing yet
	l.RunOnce(context.Background()) // named twice in a row and online - switch
	if got := len(readHubQueueMessages(t, rdb)); got != 2 {
		t.Fatalf("switch pass queued %d, want one per server (2)", got)
	}

	l.RunOnce(context.Background()) // the pass right after the switch
	msgs := readHubQueueMessages(t, rdb)
	if len(msgs) != 4 {
		t.Fatalf("after the repeat pass, queued %d in total, want 4 (2 + 2 repeated)", len(msgs))
	}
	for i, want := range []string{"srv-10", "srv-11"} {
		m := msgs[2+i]
		if m.Action != "migrate_routes" || m.ServerUUID != want || m.NewLinkToken != "generated" {
			t.Errorf("repeat message %d = %+v, want migrate_routes %s -> generated", i, m, want)
		}
	}
	if got := st.linkToken(1); got != "generated" {
		t.Errorf("link_token = %q, want still generated", got)
	}

	l.RunOnce(context.Background()) // the pass after that - settled, no more
	if got := len(readHubQueueMessages(t, rdb)); got != 4 {
		t.Errorf("queued %d once settled, want still 4 (no further repeats)", got)
	}
}

func TestEffectiveLinkToken_LearnedWinsDerivedOtherwise(t *testing.T) {
	derived := DeriveLinkToken("node-a", nodeLinkTestSecret)
	if got := effectiveLinkToken(&models.Node{Token: "node-a"}, nodeLinkTestSecret); got != derived {
		t.Errorf("no learned token: got %q, want the derived %q", got, derived)
	}
	if got := effectiveLinkToken(&models.Node{Token: "node-a", LinkToken: "generated"}, nodeLinkTestSecret); got != "generated" {
		t.Errorf("learned token: got %q, want generated", got)
	}
}
