package services

import (
	"reflect"
	"testing"
)

func TestCNAMETargetsExpandsTheLabel(t *testing.T) {
	for _, c := range []struct {
		name  string
		label string
		bases []string
		want  []string
	}{
		{"one base", "route", []string{"eu.dylaris.com"}, []string{"route.eu.dylaris.com"}},
		{"one per region", "route", []string{"eu.dylaris.com", "us.dylaris.com"},
			[]string{"route.eu.dylaris.com", "route.us.dylaris.com"}},
		{"case and space", "  Route ", []string{" EU.Dylaris.com "}, []string{"route.eu.dylaris.com"}},
		{"duplicate bases collapse", "route", []string{"eu.dylaris.com", "eu.dylaris.com"},
			[]string{"route.eu.dylaris.com"}},
		{"no label, no targets", "", []string{"eu.dylaris.com"}, nil},
		{"no bases, no targets", "route", nil, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := CNAMETargets(c.label, c.bases)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("CNAMETargets(%q, %v) = %v, want %v", c.label, c.bases, got, c.want)
			}
		})
	}
}
