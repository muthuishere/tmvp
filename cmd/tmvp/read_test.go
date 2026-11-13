package main

import (
	"testing"

	"github.com/muthuishere/tmvp/internal/config"
)

func TestParseTarget(t *testing.T) {
	cfg, err := config.Parse([]byte(`
product: demo
envs:
  dev: { systems: { db: { type: postgres }, cache: { type: redis } } }
  prod: { systems: { db: { type: postgres } } }
`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args   []string
		labels bool
		want   target
		bad    bool
	}{
		{args: []string{"db", "query", "select 1"}, want: target{Env: "all", System: "db", Verb: "query", Args: []string{"select 1"}}},
		{args: []string{"prod", "db", "health"}, want: target{Env: "prod", System: "db", Verb: "health"}},
		{args: []string{"dev,prod", "db", "health"}, want: target{Env: "dev,prod", System: "db", Verb: "health"}},
		{args: []string{"all", "db", "health"}, want: target{Env: "all", System: "db", Verb: "health"}},
		{args: []string{"prod.db", "health"}, want: target{Env: "prod", System: "db", Verb: "health"}},
		{args: []string{"*.db", "health"}, want: target{Env: "*", System: "db", Verb: "health"}},
		{args: []string{"prod.*", "health"}, want: target{Env: "prod", System: "*", Verb: "health"}},
		{args: []string{"prod", "health"}, labels: true, want: target{Env: "prod", System: "*", Verb: "health"}},
		{args: []string{"prod", "health"}, bad: true},
		{args: []string{"nope", "x"}, bad: true},
		{args: []string{"db"}, bad: true},
		{args: []string{"prod", "db", "@disk", "x"}, want: target{Env: "prod", System: "db", Verb: "@disk", Args: []string{"x"}}},
	}
	for _, tc := range cases {
		got, err := parseTarget(cfg, tc.args, tc.labels)
		if tc.bad {
			if err == nil {
				t.Errorf("%v: expected error", tc.args)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", tc.args, err)
			continue
		}
		if got.Env != tc.want.Env || got.System != tc.want.System || got.Verb != tc.want.Verb || len(got.Args) != len(tc.want.Args) {
			t.Errorf("%v: got %+v want %+v", tc.args, got, tc.want)
		}
	}
}
