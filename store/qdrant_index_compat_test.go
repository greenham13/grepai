package store

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type indexCompatServer struct {
	qdrant.UnimplementedCollectionsServer
	mode     string
	requests []*qdrant.CreateFieldIndexCollection
}

func (s *indexCompatServer) CollectionExists(context.Context, *qdrant.CollectionExistsRequest) (*qdrant.CollectionExistsResponse, error) {
	return &qdrant.CollectionExistsResponse{Result: &qdrant.CollectionExists{Exists: true}}, nil
}
func (s *indexCompatServer) Get(context.Context, *qdrant.GetCollectionInfoRequest) (*qdrant.GetCollectionInfoResponse, error) {
	if s.mode == "read error" {
		return nil, status.Error(codes.Unavailable, "schema unavailable")
	}
	kind := qdrant.PayloadSchemaType_Keyword
	if s.mode == "odd schema" {
		kind = qdrant.PayloadSchemaType_Text
	}
	return &qdrant.GetCollectionInfoResponse{Result: &qdrant.CollectionInfo{PayloadSchema: map[string]*qdrant.PayloadSchemaInfo{
		"file_path": {DataType: kind}, "content_hash": {DataType: qdrant.PayloadSchemaType_Keyword},
	}}}, nil
}

type indexCompatPoints struct {
	qdrant.UnimplementedPointsServer
	owner *indexCompatServer
}

func (p *indexCompatPoints) CreateFieldIndex(_ context.Context, r *qdrant.CreateFieldIndexCollection) (*qdrant.PointsOperationResponse, error) {
	s := p.owner
	s.requests = append(s.requests, r)
	if s.mode == "write error" {
		return nil, status.Error(codes.InvalidArgument, "prefix unsupported")
	}
	return &qdrant.PointsOperationResponse{Result: &qdrant.UpdateResult{Status: qdrant.UpdateStatus_Acknowledged}}, nil
}
func TestQdrantIndexSetupCompatibility(t *testing.T) {
	for _, mode := range []string{"read error", "odd schema", "write error", "async upgrade"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			mock := &indexCompatServer{mode: mode}
			qdrant.RegisterCollectionsServer(server, mock)
			qdrant.RegisterPointsServer(server, &indexCompatPoints{owner: mock})
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
			s := &QdrantStore{client: client, collectionName: "existing"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.ensureCollection(ctx); err != nil {
				t.Fatalf("optional index broke open: %v", err)
			}
			want := 0
			if mode == "write error" || mode == "async upgrade" {
				want = 1
			}
			if len(mock.requests) != want {
				t.Fatalf("index writes=%d want %d", len(mock.requests), want)
			}
			for _, r := range mock.requests {
				if r.GetWait() {
					t.Fatal("existing prefix upgrade blocks store open")
				}
			}
		})
	}
}
