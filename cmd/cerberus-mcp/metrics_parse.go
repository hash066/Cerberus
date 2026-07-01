package main

// metrics_parse.go turns the daemon's raw Prometheus text exposition (see
// daemon/metrics.ContentType) into structured samples, so the cerberus_metrics
// MCP tool can return real structured data (as the task calls for) rather than
// a JSON-string-wrapped text blob. This is a minimal line-oriented reader for
// the exposition format (https://prometheus.io/docs/instrumenting/exposition_formats/)
// — it does not attempt full parity with a real Prometheus text parser (no
// histogram/summary bucket reconstruction), which is fine for surfacing gauges
// and counters to an MCP client.

import (
	"strconv"
	"strings"
)

// MetricSample is one parsed exposition line: a metric name, its label set,
// and its float value.
type MetricSample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

// parseMetricsText parses Prometheus text exposition into samples, skipping
// comments (# HELP / # TYPE) and blank lines. Malformed lines are skipped
// rather than erroring the whole tool call — partial structured data beats
// none for an operator poking at a live daemon.
func parseMetricsText(text string) []MetricSample {
	var out []MetricSample
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, valueStr, ok := splitSampleLine(line)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			continue
		}
		out = append(out, MetricSample{Name: name, Labels: labels, Value: v})
	}
	return out
}

// splitSampleLine splits `name{k="v",...} value` (labels optional) into its
// parts. The value is always the last whitespace-separated token; everything
// before it is the metric name plus an optional brace-delimited label set.
func splitSampleLine(line string) (name string, labels map[string]string, value string, ok bool) {
	sp := strings.LastIndexByte(line, ' ')
	if sp < 0 {
		return "", nil, "", false
	}
	head := strings.TrimSpace(line[:sp])
	value = strings.TrimSpace(line[sp+1:])
	if head == "" || value == "" {
		return "", nil, "", false
	}
	brace := strings.IndexByte(head, '{')
	if brace < 0 {
		return head, nil, value, true
	}
	name = head[:brace]
	if !strings.HasSuffix(head, "}") {
		return "", nil, "", false
	}
	labelBody := head[brace+1 : len(head)-1]
	labels = parseLabels(labelBody)
	return name, labels, value, true
}

// parseLabels parses `k1="v1",k2="v2"` into a map. Values may contain escaped
// quotes/backslashes per the exposition format; this handles the common case
// (no escapes) and falls back to a best-effort split otherwise.
func parseLabels(body string) map[string]string {
	labels := map[string]string{}
	if strings.TrimSpace(body) == "" {
		return labels
	}
	var (
		key     strings.Builder
		val     strings.Builder
		inValue bool
		inQuote bool
	)
	flush := func() {
		k := strings.TrimSpace(key.String())
		if k != "" {
			labels[k] = val.String()
		}
		key.Reset()
		val.Reset()
		inValue = false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case !inValue && c == '=':
			inValue = true
		case inValue && c == '"' && !inQuote:
			inQuote = true
		case inValue && c == '"' && inQuote:
			inQuote = false
		case !inQuote && c == ',':
			flush()
		default:
			if inValue {
				if inQuote {
					val.WriteByte(c)
				}
			} else {
				key.WriteByte(c)
			}
		}
	}
	flush()
	return labels
}
