package main

import "testing"

func TestParseMetricsText(t *testing.T) {
	text := `# HELP cerberus_peers Connected mesh peers
# TYPE cerberus_peers gauge
cerberus_peers 3
# TYPE cerberus_wasm_execs_total counter
cerberus_wasm_execs_total{status="ok"} 42
cerberus_wasm_execs_total{status="error",peer="a,b"} 1

malformed line without a value
`
	samples := parseMetricsText(text)
	if len(samples) != 3 {
		t.Fatalf("got %d samples, want 3: %+v", len(samples), samples)
	}

	byName := map[string][]MetricSample{}
	for _, s := range samples {
		byName[s.Name] = append(byName[s.Name], s)
	}

	peers := byName["cerberus_peers"]
	if len(peers) != 1 || peers[0].Value != 3 || len(peers[0].Labels) != 0 {
		t.Fatalf("unexpected cerberus_peers samples: %+v", peers)
	}

	execs := byName["cerberus_wasm_execs_total"]
	if len(execs) != 2 {
		t.Fatalf("unexpected cerberus_wasm_execs_total samples: %+v", execs)
	}
	foundOK, foundErr := false, false
	for _, s := range execs {
		switch s.Labels["status"] {
		case "ok":
			foundOK = true
			if s.Value != 42 {
				t.Errorf("ok sample value = %v, want 42", s.Value)
			}
		case "error":
			foundErr = true
			if s.Value != 1 {
				t.Errorf("error sample value = %v, want 1", s.Value)
			}
			if s.Labels["peer"] != "a,b" {
				t.Errorf("comma-in-value label = %q, want %q", s.Labels["peer"], "a,b")
			}
		}
	}
	if !foundOK || !foundErr {
		t.Fatalf("missing expected labeled samples: %+v", execs)
	}
}

func TestParseMetricsTextEmpty(t *testing.T) {
	if got := parseMetricsText(""); len(got) != 0 {
		t.Fatalf("parseMetricsText(\"\") = %+v, want empty", got)
	}
	if got := parseMetricsText("# just a comment\n"); len(got) != 0 {
		t.Fatalf("parseMetricsText(comment-only) = %+v, want empty", got)
	}
}
