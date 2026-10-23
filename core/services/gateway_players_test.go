package services

import (
	"reflect"
	"testing"
	"time"
)

func TestSumFreshPlayers(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	got := sumFreshPlayers([]edgePlayersSample{
		{at: now.Add(-2 * time.Second), players: map[string]int{"a": 2, "b": 1}},
		{at: now.Add(-8 * time.Second), players: map[string]int{"a": 1}},
		// A dead edge's last sample: its players are on the sibling by now.
		{at: now.Add(-30 * time.Second), players: map[string]int{"a": 5, "c": 4}},
		{at: now, players: map[string]int{"d": 0}},
		{at: now, players: nil},
	}, now)
	if want := map[string]int{"a": 3, "b": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestStreamIDTime(t *testing.T) {
	at, ok := streamIDTime("1729700000123-4")
	if !ok || at.UnixMilli() != 1729700000123 {
		t.Fatalf("got %v %v", at, ok)
	}
	if _, ok := streamIDTime("garbage"); ok {
		t.Fatal("a malformed id parsed")
	}
}
