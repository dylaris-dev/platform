package store

import (
	"fmt"
	"testing"

	"github.com/lib/pq"
)

func TestIsUniqueViolationOfMatchesTheConstraintName(t *testing.T) {
	for _, c := range []struct {
		name       string
		err        error
		constraint string
		want       bool
	}{
		{"voice index", &pq.Error{Code: "23505", Constraint: remoteOneVoiceIndex}, remoteOneVoiceIndex, true},
		{"bedrock index", fmt.Errorf("wrapped: %w", &pq.Error{Code: "23505", Constraint: remoteOneBedrockIndex}), remoteOneBedrockIndex, true},
		{"bedrock hit, voice asked", &pq.Error{Code: "23505", Constraint: remoteOneBedrockIndex}, remoteOneVoiceIndex, false},
		{"voice hit, bedrock asked", &pq.Error{Code: "23505", Constraint: remoteOneVoiceIndex}, remoteOneBedrockIndex, false},
		{"check violation", &pq.Error{Code: "23514", Constraint: remoteOneBedrockIndex}, remoteOneBedrockIndex, false},
		{"no error", nil, remoteOneVoiceIndex, false},
	} {
		if got := isUniqueViolationOf(c.err, c.constraint); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
