package handlers

import (
	"context"
	"errors"
	"testing"
)

// Renders hold a stored zip and the pack being built in memory; with nothing
// bounding them, parallel exports or share-link hits could take Core down. A
// render past the limit waits, and gives up with its request.
func TestMrpackRendersAreBounded(t *testing.T) {
	for i := 0; i < cap(mrpackRenders); i++ {
		mrpackRenders <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(mrpackRenders); i++ {
			<-mrpackRenders
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &PacksHandler{state: &AppState{}}
	if _, err := h.renderMrpack(ctx, nil, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a render past the limit ran: %v", err)
	}
}
