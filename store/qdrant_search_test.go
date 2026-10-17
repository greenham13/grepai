package store

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type pathSearchServer struct {
	qdrant.UnimplementedPointsServer
	query func(*qdrant.QueryPoints) (*qdrant.QueryResponse, error)
}

func (s *pathSearchServer) Query(_ context.Context, request *qdrant.QueryPoints) (*qdrant.QueryResponse, error) {
	return s.query(request)
}

func TestQdrantSearchPathFilter(t *testing.T) {
	for _, tc := range []struct {
		prefix, collection string
		filter             bool
	}{
		{"", "test_path_filter", false},
		{"ws/", "workspace_ws", false},
		{"ws/", "shared_collection", true},
		{"src/", "folder_collection", true},
		{"ws/project/src", "workspace_ws", true},
		{"ws/project/with space", "workspace_ws", true},
		{"ws/project/日本語", "workspace_ws", true},
	} {
		prefix := tc.prefix
		t.Run(tc.collection+"/"+prefix, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			fail := false
			qdrant.RegisterPointsServer(server, &pathSearchServer{query: func(request *qdrant.QueryPoints) (*qdrant.QueryResponse, error) {
				if !tc.filter {
					wantLimit := uint64(2)
					if prefix != "" {
						wantLimit = 4
					}
					if request.GetFilter() != nil || request.GetLimit() != wantLimit {
						t.Errorf("unscoped request: %v", request)
					}
				} else {
					must := request.GetFilter().GetMust()
					if len(must) != 1 || must[0].GetField().GetKey() != "file_path" || must[0].GetField().GetMatch().GetText() != prefix || request.GetLimit() != 4 {
						t.Errorf("scoped request: %v", request)
					}
				}
				if fail {
					return nil, status.Error(codes.Unavailable, "query failed")
				}
				paths := []string{prefix + "/a.go", prefix + "/b.go"}
				if prefix != "" {
					paths = append([]string{"other/" + prefix + "/wrong.go"}, paths...)
				}
				var points []*qdrant.ScoredPoint
				for _, p := range paths {
					points = append(points, &qdrant.ScoredPoint{Payload: map[string]*qdrant.Value{"file_path": mustCreateValue(t, p)}})
				}
				return &qdrant.QueryResponse{Result: points}, nil
			}})
			t.Cleanup(server.Stop)
			go func() {
				if err := server.Serve(listener); err != nil {
					t.Error(err)
				}
			}()
			client, err := qdrant.NewClient(&qdrant.Config{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, SkipCompatibilityCheck: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			})
			s := &QdrantStore{client: client, collectionName: tc.collection}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			results, err := s.Search(ctx, []float32{1, 0}, 2, SearchOptions{PathPrefix: prefix})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 2 || results[0].Chunk.FilePath != prefix+"/a.go" || results[1].Chunk.FilePath != prefix+"/b.go" {
				t.Fatalf("wrong results: %+v", results)
			}
			fail = true
			if _, err := s.Search(ctx, []float32{1, 0}, 2, SearchOptions{PathPrefix: prefix}); err == nil || !strings.Contains(err.Error(), "query failed") {
				t.Fatalf("lost query error: %v", err)
			}
		})
	}
}

// Opt in against a local Qdrant; only this unique collection is ever written.
func TestQdrantSearchPathFilterLive(t *testing.T) {
	if os.Getenv("GREPAI_TEST_QDRANT") != "1" {
		t.Skip("set GREPAI_TEST_QDRANT=1 for local Qdrant on port 6334")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := "test_grepai_path_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	s, err := NewQdrantStore(ctx, "localhost", 6334, false, name, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.client.DeleteCollection(ctx, name); err != nil {
			t.Error(err)
		}
		if err := s.client.Close(); err != nil {
			t.Error(err)
		}
	})
	var points []*qdrant.PointStruct
	add := func(p string, v []float32) {
		payload, err := s.buildChunkPayload(Chunk{FilePath: p})
		if err != nil {
			t.Fatal(err)
		}
		points = append(points, &qdrant.PointStruct{Id: qdrant.NewIDNum(uint64(len(points) + 1)), Vectors: qdrant.NewVectors(v...), Payload: payload})
	}
	for i := 0; i < 100; i++ {
		add(fmt.Sprintf("ws/other/%d.go", i), []float32{1, 0})
	}
	prefixes := []string{"ws/project/src", "ws/project/with space", "ws/project/日本語"}
	for _, prefix := range prefixes {
		add("other/"+prefix+"/wrong.go", []float32{1, 0})
		add(prefix+"/best.go", []float32{0.8, 0.2})
		add(prefix+"/next.go", []float32{0.6, 0.4})
	}
	_, err = s.client.Upsert(ctx, &qdrant.UpsertPoints{CollectionName: name, Wait: qdrant.PtrOf(true), Points: points})
	if err != nil {
		t.Fatal(err)
	}
	// Reopen existing data: no embedding or payload migration is needed.
	if err := s.ensureCollection(ctx); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range append(prefixes, "ws/missing") {
		results, err := s.Search(ctx, []float32{1, 0}, 2, SearchOptions{PathPrefix: prefix})
		if err != nil {
			t.Fatal(err)
		}
		if prefix == "ws/missing" {
			if len(results) != 0 {
				t.Fatalf("missing path returned %+v", results)
			}
			continue
		}
		if len(results) != 2 || results[0].Chunk.FilePath != prefix+"/best.go" || results[1].Chunk.FilePath != prefix+"/next.go" {
			t.Fatalf("%q returned %+v", prefix, results)
		}
	}
	results, err := s.Search(ctx, []float32{1, 0}, 2, SearchOptions{})
	if err != nil || len(results) != 2 || results[0].Score < 0.99 {
		t.Fatalf("unscoped: %+v %v", results, err)
	}
}
