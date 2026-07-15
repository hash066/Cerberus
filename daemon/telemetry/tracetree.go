package telemetry

// tracetree.go assembles a flat set of exported spans into the nested span tree
// the CLI and tray dashboard render. The OTel pipeline in tracing.go exports
// finished spans one-by-one (parent/child linked only by span ids); a renderer
// needs them stitched back into the workflow tree described in
// docs/verticals/08 §3 (agent → scheduler placement → shard exec → 9P grant →
// settlement).
//
// The assembler is deliberately transport-agnostic: it works on a plain Span
// value (trace_id, span_id, parent_id, name, timing, attrs) so the same code
// serves spans decoded off the Zenoh otel topic, the log exporter, or an
// in-memory test exporter. Converting an SDK ReadOnlySpan into this form is a
// one-liner (FromReadOnlySpan) so callers on the export path don't reimplement
// the walk.

import (
	"sort"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Span is the minimal, transport-agnostic shape the tree assembler consumes.
// SpanID is unique within a trace; ParentID is empty ("") for a root span.
type Span struct {
	TraceID  string            `json:"trace_id"`
	SpanID   string            `json:"span_id"`
	ParentID string            `json:"parent_id"`
	Name     string            `json:"name"`
	StartNs  int64             `json:"start_ns"`
	EndNs    int64             `json:"end_ns"`
	Status   string            `json:"status,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"`
}

// DurationNs is the span's wall-clock duration in nanoseconds.
func (s Span) DurationNs() int64 {
	if s.EndNs < s.StartNs {
		return 0
	}
	return s.EndNs - s.StartNs
}

// TreeNode is one node in the assembled span tree, with its children attached.
// Children are ordered by start time (then span id) so a render is stable.
type TreeNode struct {
	Span     Span        `json:"span"`
	Children []*TreeNode `json:"children,omitempty"`
}

// FromReadOnlySpan converts an SDK ReadOnlySpan (what tracing.go's exporter
// receives) into the transport-agnostic Span the assembler consumes. The parent
// id is empty when the span is a trace root (no valid parent span context).
func FromReadOnlySpan(s sdktrace.ReadOnlySpan) Span {
	sc := s.SpanContext()
	parent := ""
	if p := s.Parent(); p.HasSpanID() {
		parent = p.SpanID().String()
	}
	attrs := map[string]string{}
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	return Span{
		TraceID:  sc.TraceID().String(),
		SpanID:   sc.SpanID().String(),
		ParentID: parent,
		Name:     s.Name(),
		StartNs:  s.StartTime().UnixNano(),
		EndNs:    s.EndTime().UnixNano(),
		Status:   s.Status().Code.String(),
		Attrs:    attrs,
	}
}

// AssembleTrees stitches a flat span set into one or more trees.
//
// Robustness (the export path is unordered and can be incomplete):
//   - Spans may arrive in any order (a child before its parent): the walk is
//     two-pass, so ordering never matters.
//   - A span whose ParentID names a span not present in the set (an orphan —
//     e.g. the parent was sampled out, lost to a partition, or lives on another
//     node) is promoted to a root rather than dropped, so no work is hidden.
//   - A span with an empty ParentID is a root.
//   - Duplicate span ids keep the first occurrence (exporters can double-deliver
//     on retry); later duplicates are ignored.
//
// Roots are returned sorted by start time (then span id); each node's children
// are sorted the same way. The result is deterministic for a given input set.
func AssembleTrees(spans []Span) []*TreeNode {
	nodes := make(map[string]*TreeNode, len(spans))
	// Pass 1: index every span by id (first-wins on duplicates).
	for _, s := range spans {
		if s.SpanID == "" {
			continue // unaddressable; cannot be linked or be a parent
		}
		if _, dup := nodes[s.SpanID]; dup {
			continue
		}
		nodes[s.SpanID] = &TreeNode{Span: s}
	}
	// Pass 2: link each node under its parent, or promote it to a root.
	var roots []*TreeNode
	for _, n := range nodes {
		pid := n.Span.ParentID
		if pid == "" {
			roots = append(roots, n)
			continue
		}
		if parent, ok := nodes[pid]; ok && parent != n {
			parent.Children = append(parent.Children, n)
			continue
		}
		// Orphan (or a span pointing at itself): treat as a root.
		roots = append(roots, n)
	}
	sortNodes(roots)
	for _, n := range nodes {
		sortNodes(n.Children)
	}
	return roots
}

// AssembleTrace assembles only the spans matching traceID. Convenience for the
// common "show me this one workflow" call from the CLI/dashboard.
func AssembleTrace(spans []Span, traceID string) []*TreeNode {
	var sub []Span
	for _, s := range spans {
		if s.TraceID == traceID {
			sub = append(sub, s)
		}
	}
	return AssembleTrees(sub)
}

// Flatten returns the nodes of a tree in pre-order (parent before children) —
// handy for a flat indented render or a list view.
func Flatten(roots []*TreeNode) []*TreeNode {
	var out []*TreeNode
	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		out = append(out, n)
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}

func sortNodes(ns []*TreeNode) {
	sort.SliceStable(ns, func(i, j int) bool {
		if ns[i].Span.StartNs != ns[j].Span.StartNs {
			return ns[i].Span.StartNs < ns[j].Span.StartNs
		}
		return ns[i].Span.SpanID < ns[j].Span.SpanID
	})
}
