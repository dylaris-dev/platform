package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/redisacl"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// aclHook answers the ACL commands miniredis does not implement: USERS from a
// fixed list, DELUSER recorded, everything else OK.
type aclHook struct {
	users   []string
	deleted []string
}

func (h *aclHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *aclHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *aclHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if len(args) < 2 || !strings.EqualFold(fmt.Sprint(args[0]), "acl") {
			return next(ctx, cmd)
		}
		c, ok := cmd.(*redis.Cmd)
		if !ok {
			return next(ctx, cmd)
		}
		switch strings.ToUpper(fmt.Sprint(args[1])) {
		case "USERS":
			vals := make([]interface{}, len(h.users))
			for i, u := range h.users {
				vals[i] = u
			}
			c.SetVal(vals)
		case "DELUSER":
			h.deleted = append(h.deleted, fmt.Sprint(args[2]))
			c.SetVal(int64(1))
		default:
			c.SetVal("OK")
		}
		return nil
	}
}

// pruneStore answers ListNodes with `first` once, then with `later`: a node
// that enrolled on another replica while the sweep was running.
type pruneStore struct {
	first, later []models.Node
	calls        int
}

func (s *pruneStore) ListNodes() ([]models.Node, error) {
	s.calls++
	if s.calls == 1 {
		return s.first, nil
	}
	return s.later, nil
}
func (s *pruneStore) GetNodeSecretEnc(int) (string, error) { return "", nil }
func (s *pruneStore) SetNodeSecretEnc(int, string) error   { return nil }
func (s *pruneStore) SetNodeSecretEncIfUnchanged(int, string, string) (bool, error) {
	return false, nil
}
func (s *pruneStore) ListServersByNode(int) ([]models.Server, error) { return nil, nil }

// The prune compared a fresh ACL USERS against the node list read at the start
// of the sweep, so a node that enrolled in between had its brand-new users
// deleted right after pairing. Only a user that existed before the node list
// was read may be judged an orphan.
func TestACLPruneSparesANodeThatEnrolledDuringTheSweep(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	hook := &aclHook{users: []string{
		"default",
		"node-a", "node-a-link",
		"node-b", "node-b-link", // enrolled after the sweep's first ListNodes
		"node-gone", "node-gone-link", // a real orphan
	}}
	rdb.AddHook(hook)

	a := models.Node{ID: 1, Token: "a"}
	b := models.Node{ID: 2, Token: "b"}
	st := &pruneStore{first: []models.Node{a}, later: []models.Node{a, b}}
	r := &ACLReconciler{store: st, prov: redisacl.NewProvisioner(rdb), redis: rdb, clusterSecret: "cs"}

	r.reconcileOnce(context.Background())

	got := strings.Join(hook.deleted, ",")
	if got != "node-gone,node-gone-link" {
		t.Errorf("deleted %q, want only the orphan node-gone users", got)
	}
}
