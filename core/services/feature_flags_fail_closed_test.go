package services

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

type flagReader struct {
	v   string
	err error
}

func (f *flagReader) GetSetting(string) (string, error) { return f.v, f.err }

// GetFailClosed guards fences. Never saved is the default; unreadable is not
// "off", and must not be cached as off either.
func TestGetFailClosed(t *testing.T) {
	cases := []struct {
		name string
		r    *flagReader
		want bool
	}{
		{"saved on", &flagReader{v: "true"}, true},
		{"saved off", &flagReader{v: "false"}, false},
		{"never saved", &flagReader{err: sql.ErrNoRows}, false},
		{"unreadable", &flagReader{err: errors.New("connection refused")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NewFeatureFlags(c.r).GetFailClosed(context.Background(), "k", false); got != c.want {
				t.Errorf("GetFailClosed = %v, want %v", got, c.want)
			}
		})
	}

	t.Run("a fault is not cached", func(t *testing.T) {
		r := &flagReader{err: errors.New("connection refused")}
		f := NewFeatureFlags(r)
		f.GetFailClosed(context.Background(), "k", false)
		r.err, r.v = nil, "false"
		if f.GetFailClosed(context.Background(), "k", false) {
			t.Error("the fault was cached, so the fence stayed on after the database came back")
		}
	})
}
