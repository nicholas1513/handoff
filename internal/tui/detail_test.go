package tui

import (
	"strings"
	"testing"
)

func TestCommandLinesShowEveryElementVerbatim(t *testing.T) {
	cases := [][]string{
		{"az", "rest", "--method", "put", "--resource", "499b84ac-1321-427f-aa17-267ca6975798",
			"--url", "https://dev.azure.com/Org/Example%20Proj/Example%20Proj%20Team/_apis/dashboard/dashboards/3c9a1f2e-7b4d-4e8a-9f61-2d5c8b0e7a43?api-version=7.1-preview.3",
			"--body", "@{file:body.json}"},
		{"/usr/bin/true"},
		{"curl", "--data", "-", "-X", "POST", "https://x.test/a"},
		{"az", "rest", "--method=get", "--url=https://x.test/b"},
		{"bash", "-c", "echo hi; rm -rf ~/x"},
	}
	for _, argv := range cases {
		got := flatten(strings.Join(commandLines(argv, map[string]int{"body.json": 6100}), "\n"))
		for _, a := range argv {
			if !strings.Contains(got, sanitize(a)) {
				t.Errorf("argv %q: element %q missing from %q", argv, a, got)
			}
		}
	}
}

func TestMethodOf(t *testing.T) {
	for want, argv := range map[string][]string{
		"PUT": {"az", "rest", "--method", "put"}, "GET": {"az", "rest", "--method=get"},
		"POST": {"curl", "-X", "POST"}, "": {"az", "account", "show"},
	} {
		if got := methodOf(argv); got != want {
			t.Errorf("methodOf(%q) = %q want %q", argv, got, want)
		}
	}
}

func TestAttachmentDecoration(t *testing.T) {
	got := flatten(strings.Join(commandLines([]string{"az", "rest", "--body", "@{file:body.json}"}, map[string]int{"body.json": 6100}), "\n"))
	for _, w := range []string{"@{file:body.json}", "📎", "6.1 KB"} {
		if !strings.Contains(got, w) {
			t.Errorf("%q missing from %q", w, got)
		}
	}
}
