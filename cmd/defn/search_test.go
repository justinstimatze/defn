package main

import "testing"

func TestParseSearchArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantPattern string
		wantRank    bool
		wantJSON    bool
		wantLimit   int
		wantErr     bool
	}{
		{name: "bare pattern", args: []string{"render"}, wantPattern: "render", wantLimit: 20},
		{name: "rank flag", args: []string{"--rank", "render"}, wantPattern: "render", wantRank: true, wantLimit: 20},
		{name: "json flag", args: []string{"--json", "Render"}, wantPattern: "Render", wantJSON: true, wantLimit: 20},
		{name: "limit value", args: []string{"--limit", "5", "render"}, wantPattern: "render", wantLimit: 5},
		{name: "all flags", args: []string{"--rank", "--json", "--limit", "3", "Handler"}, wantPattern: "Handler", wantRank: true, wantJSON: true, wantLimit: 3},
		{name: "pattern before flags", args: []string{"render", "--rank"}, wantPattern: "render", wantRank: true, wantLimit: 20},
		{name: "wildcard pattern", args: []string{"%Render%"}, wantPattern: "%Render%", wantLimit: 20},

		{name: "no pattern", args: []string{}, wantErr: true},
		{name: "only flags", args: []string{"--rank"}, wantErr: true},
		{name: "limit missing value", args: []string{"--limit"}, wantErr: true},
		{name: "limit zero", args: []string{"--limit", "0", "render"}, wantErr: true},
		{name: "limit negative", args: []string{"--limit", "-5", "render"}, wantErr: true},
		{name: "limit non-numeric", args: []string{"--limit", "abc", "render"}, wantErr: true},
		{name: "unknown flag", args: []string{"--mystery", "render"}, wantErr: true},
		{name: "two positionals", args: []string{"foo", "bar"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pattern, rank, jsonFlag, limit, err := parseSearchArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got pattern=%q rank=%v json=%v limit=%d", pattern, rank, jsonFlag, limit)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pattern != tc.wantPattern {
				t.Errorf("pattern = %q, want %q", pattern, tc.wantPattern)
			}
			if rank != tc.wantRank {
				t.Errorf("rank = %v, want %v", rank, tc.wantRank)
			}
			if jsonFlag != tc.wantJSON {
				t.Errorf("json = %v, want %v", jsonFlag, tc.wantJSON)
			}
			if limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", limit, tc.wantLimit)
			}
		})
	}
}
