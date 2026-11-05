package main

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// H47: Core lets a server keep container port 25565 next to a different global
// game port, so the egress chain must not drop it between servers - or a
// tenant's proxy cannot reach a backend that still listens on 25565.
func TestThePortSetSparesTheDefaultGamePort(t *testing.T) {
	cases := []struct {
		game int
		want string
	}{
		{25565, "5432, 6379, 25500-25564, 25566-25599"},
		{25570, "5432, 6379, 25500-25564, 25566-25569, 25571-25599"},
		{25566, "5432, 6379, 25500-25564, 25567-25599"},
		{25567, "5432, 6379, 25500-25564, 25566, 25568-25599"},
		{30000, "5432, 6379, 25500-25564, 25566-25599"},
		{25500, "5432, 6379, 25501-25564, 25566-25599"},
		{25599, "5432, 6379, 25500-25564, 25566-25598"},
	}
	for _, c := range cases {
		if got := platformPortSet(c.game); got != c.want {
			t.Errorf("platformPortSet(%d) = %q, want %q", c.game, got, c.want)
		}
	}
}

type cmdOrder struct{ keys *[]string }

func (cmdOrder) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h cmdOrder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if strings.EqualFold(cmd.Name(), "get") {
			*h.keys = append(*h.keys, cmd.Args()[1].(string))
		}
		return next(ctx, cmd)
	}
}
func (cmdOrder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// H49: Core writes the policy and THEN the tenancy flag, so a node that reads
// the flag first can never pair a new "shared" flag with the older, narrower
// policy. Reading them the other way round could.
func TestTheTenancyFlagIsReadBeforeThePolicy(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	var keys []string
	rdb.AddHook(cmdOrder{&keys})

	dm := &DockerManager{external: true}
	dm.planNetPolicy(context.Background(), rdb, "tok")
	flag, pol := -1, -1
	for i, k := range keys {
		switch k {
		case netPolicyTenancyKey("tok"):
			flag = i
		case netPolicyKey("tok"):
			pol = i
		}
	}
	if flag < 0 || pol < 0 || flag > pol {
		t.Fatalf("read order %v: the tenancy flag must come before the policy", keys)
	}
}
