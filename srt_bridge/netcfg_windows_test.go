package main

import "testing"

func TestRuleCovers(t *testing.T) {
	out := func(ports string) string {
		return "Rule Name:                            SRT Bridge\r\n" +
			"Enabled:                              Yes\r\n" +
			"Protocol:                             UDP\r\n" +
			"LocalPort:                            " + ports + "\r\n" +
			"RemotePort:                           Any\r\n"
	}
	cases := []struct {
		rule, need string
		want       bool
	}{
		{"21101", "21101", true},
		{"21101-21108", "21101", true}, // switched back to one port: still open
		{"21101-21108", "21101-21108", true},
		{"21101", "21101-21108", false},
		{"21101-21104", "21101-21108", false},
		{"21101-21104,21105-21108", "21101-21108", true},
		{"9001", "21101", false},
		{"121101", "21101", false},
		{"Any", "21101", false},
	}
	for _, c := range cases {
		if got := ruleCovers(out(c.rule), c.need); got != c.want {
			t.Errorf("rule %q, needed %q: got %v, want %v", c.rule, c.need, got, c.want)
		}
	}
}
