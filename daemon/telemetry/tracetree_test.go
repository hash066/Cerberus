package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// childNames returns the child span names of n (order as assembled).
func childNames(n *TreeNode) []string {
	out := make([]string, 0, len(n.Children))
	for _, c := range n.Children {
		out = append(out, c.Span.Name)
	}
	return out
}

// findRoot returns the root with the given span name, or nil.
func findRoot(roots []*TreeNode, name string) *TreeNode {
	for _, r := range roots {
		if r.Span.Name == name {
			return r
		}
	}
	return nil
}

// TestAssembleNestsParentChild covers the happy path plus deliberately
// out-of-order input: the agent→placement→exec workflow from vertical 08, with
// the leaf delivered before its parent, proving the two-pass walk is
// order-independent.
func TestAssembleNestsParentChild(t *testing.T) {
	// Tree:  agent (root) -> placement -> exec
	//                     -> settlement
	spans := []Span{
		// Intentionally out of order: child "exec" appears before "placement".
		{TraceID: "t1", SpanID: "exec", ParentID: "placement", Name: "shard.exec", StartNs: 30, EndNs: 40},
		{TraceID: "t1", SpanID: "agent", ParentID: "", Name: "agent.run", StartNs: 10, EndNs: 100},
		{TraceID: "t1", SpanID: "settlement", ParentID: "agent", Name: "settle", StartNs: 50, EndNs: 60},
		{TraceID: "t1", SpanID: "placement", ParentID: "agent", Name: "scheduler.place", StartNs: 20, EndNs: 45},
	}

	roots := AssembleTrees(spans)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	agent := roots[0]
	if agent.Span.Name != "agent.run" {
		t.Fatalf("unexpected root %q", agent.Span.Name)
	}
	// Children sorted by start time: placement(20) before settlement(50).
	if got := childNames(agent); len(got) != 2 || got[0] != "scheduler.place" || got[1] != "settle" {
		t.Fatalf("agent children wrong/unsorted: %v", got)
	}
	placement := agent.Children[0]
	if got := childNames(placement); len(got) != 1 || got[0] != "shard.exec" {
		t.Fatalf("placement should own shard.exec, got %v", got)
	}
	if d := placement.Children[0].Span.DurationNs(); d != 10 {
		t.Fatalf("exec duration: want 10, got %d", d)
	}
	// Whole tree should flatten to all four spans (pre-order).
	if n := len(Flatten(roots)); n != 4 {
		t.Fatalf("flatten should yield 4 nodes, got %d", n)
	}
}

// TestAssembleOrphanBecomesRoot proves a span whose parent is absent (sampled
// out / lost to a partition / on another node) is promoted to a root rather than
// silently dropped, and still keeps its own subtree.
func TestAssembleOrphanBecomesRoot(t *testing.T) {
	spans := []Span{
		{TraceID: "t1", SpanID: "root", ParentID: "", Name: "root", StartNs: 0, EndNs: 100},
		{TraceID: "t1", SpanID: "child", ParentID: "root", Name: "child", StartNs: 5, EndNs: 50},
		// orphan's parent "ghost" is not in the set.
		{TraceID: "t1", SpanID: "orphan", ParentID: "ghost", Name: "orphan", StartNs: 200, EndNs: 300},
		// the orphan itself parents a grandchild — subtree must survive.
		{TraceID: "t1", SpanID: "grand", ParentID: "orphan", Name: "grand", StartNs: 210, EndNs: 250},
	}

	roots := AssembleTrees(spans)
	if len(roots) != 2 {
		t.Fatalf("expected 2 roots (real root + promoted orphan), got %d", len(roots))
	}
	// Roots sorted by start: root(0) before orphan(200).
	if roots[0].Span.Name != "root" || roots[1].Span.Name != "orphan" {
		t.Fatalf("roots unsorted/wrong: %s, %s", roots[0].Span.Name, roots[1].Span.Name)
	}
	orphan := findRoot(roots, "orphan")
	if orphan == nil || len(orphan.Children) != 1 || orphan.Children[0].Span.Name != "grand" {
		t.Fatalf("orphan subtree not preserved: %+v", orphan)
	}
	// No span should be lost.
	if n := len(Flatten(roots)); n != 4 {
		t.Fatalf("flatten should yield 4 nodes, got %d", n)
	}
}

// TestAssembleDuplicateSpanIDFirstWins proves a doubled-delivered span (exporter
// retry) does not create a duplicate node.
func TestAssembleDuplicateSpanIDFirstWins(t *testing.T) {
	spans := []Span{
		{TraceID: "t1", SpanID: "a", Name: "first", StartNs: 0, EndNs: 1},
		{TraceID: "t1", SpanID: "a", Name: "second-copy", StartNs: 0, EndNs: 1},
	}
	roots := AssembleTrees(spans)
	if len(roots) != 1 || roots[0].Span.Name != "first" {
		t.Fatalf("duplicate not deduped (first-wins): %+v", roots)
	}
}

// TestAssembleTraceFilters proves AssembleTrace only assembles the requested
// trace and ignores spans from other traces.
func TestAssembleTraceFilters(t *testing.T) {
	spans := []Span{
		{TraceID: "t1", SpanID: "a", Name: "a", StartNs: 0, EndNs: 1},
		{TraceID: "t2", SpanID: "b", Name: "b", StartNs: 0, EndNs: 1},
	}
	roots := AssembleTrace(spans, "t1")
	if len(roots) != 1 || roots[0].Span.Name != "a" {
		t.Fatalf("AssembleTrace did not filter by trace id: %+v", roots)
	}
}

// TestFromReadOnlySpanRoundTrips proves the bridge from the real SDK export path
// (ReadOnlySpan) into the assembler's Span: a parent/child pair recorded through
// a real TracerProvider assembles into the correct tree.
func TestFromReadOnlySpanRoundTrips(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tr := tp.Tracer("test")

	ctx, parent := tr.Start(context.Background(), "parent")
	_, child := tr.Start(ctx, "child")
	child.End()
	parent.End()

	var spans []Span
	for _, ro := range exp.GetSpans().Snapshots() {
		spans = append(spans, FromReadOnlySpan(ro))
	}
	if len(spans) != 2 {
		t.Fatalf("expected 2 recorded spans, got %d", len(spans))
	}

	roots := AssembleTrees(spans)
	if len(roots) != 1 || roots[0].Span.Name != "parent" {
		t.Fatalf("expected single 'parent' root, got %+v", roots)
	}
	if got := childNames(roots[0]); len(got) != 1 || got[0] != "child" {
		t.Fatalf("expected parent->child nesting, got %v", got)
	}
}
