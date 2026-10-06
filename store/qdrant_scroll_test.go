package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// TestQdrantScrollPagesStreamsAndStopsOnVisitError verifies scrollPages hands
// each page to the visitor as it arrives (never an empty page), and that a
// visitor error stops the scroll without requesting further pages.
func TestQdrantScrollPagesStreamsAndStopsOnVisitError(t *testing.T) {
	pages := []*qdrant.ScrollResponse{
		{Result: []*qdrant.RetrievedPoint{scrollPagePoint(t, 1, "a.go"), scrollPagePoint(t, 2, "b.go")}, NextPageOffset: qdrant.NewIDNum(2)},
		{Result: []*qdrant.RetrievedPoint{scrollPagePoint(t, 3, "c.go")}, NextPageOffset: qdrant.NewIDNum(3)},
		{Result: nil, NextPageOffset: qdrant.NewIDNum(4)},
	}
	server := &scrollPagesServer{pages: pages}
	s, ctx := newScrollTestStore(t, server)

	var sizes []int
	err := s.scrollPages(ctx, nil, []string{"file_path"}, func(points []*qdrant.RetrievedPoint) error {
		sizes = append(sizes, len(points))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sizes, []int{2, 1}) {
		t.Fatalf("visited page sizes = %v, want [2 1]", sizes)
	}
	if len(server.requests) != 3 {
		t.Fatalf("scroll called %d times, want 3", len(server.requests))
	}

	stop := errors.New("stop")
	server2 := &scrollPagesServer{pages: pages}
	s2, ctx2 := newScrollTestStore(t, server2)
	visits := 0
	err = s2.scrollPages(ctx2, nil, []string{"file_path"}, func([]*qdrant.RetrievedPoint) error {
		visits++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("scrollPages err = %v, want visitor error", err)
	}
	if visits != 1 || len(server2.requests) != 1 {
		t.Fatalf("visits=%d requests=%d, want 1 and 1", visits, len(server2.requests))
	}
}
