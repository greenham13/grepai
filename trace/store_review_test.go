package trace

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func saveReviewFile(t *testing.T, s *GOBSymbolStore, file string) {
	t.Helper()
	syms, refs := testFileEntries(file, 0)
	if err := s.SaveFile(context.Background(), file, syms, refs); err != nil {
		t.Fatal(err)
	}
	if err := s.Persist(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSymbolTwoWritersKeepChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "symbols.gob")
	a := NewGOBSymbolStore(path)
	saveReviewFile(t, a, "base.go")
	b := NewGOBSymbolStore(path)
	if err := b.Load(ctx); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		s    *GOBSymbolStore
		file string
	}{
		{a, "a1.go"}, {a, "a2.go"}, {b, "b1.go"}, {a, "a3.go"}, {b, "b2.go"},
	} {
		saveReviewFile(t, step.s, step.file)
	}
	check := NewGOBSymbolStore(path)
	if err := check.Load(ctx); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"base.go", "a1.go", "a2.go", "b1.go", "a3.go", "b2.go"} {
		if !check.IsFileIndexed(file) {
			t.Fatalf("lost %s", file)
		}
	}
}

func TestSymbolBadFinalFrameIsTorn(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("bad gob")} {
		t.Run(string(payload), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "symbols.gob")
			s := NewGOBSymbolStore(path)
			saveReviewFile(t, s, "base.go")
			saveReviewFile(t, s, "good.go")
			frame := make([]byte, 8)
			binary.LittleEndian.PutUint32(frame, uint32(len(payload)))
			binary.LittleEndian.PutUint32(frame[4:], crc32.Checksum(payload, symbolLogCRC))
			f, err := os.OpenFile(path+".log", os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write(append(frame, payload...)); err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			check := NewGOBSymbolStore(path)
			if err := check.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !check.IsFileIndexed("good.go") {
				t.Fatal("lost good frame")
			}
			saveReviewFile(t, check, "next.go")
			if err := s.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !s.IsFileIndexed("next.go") {
				t.Fatal("lost next save")
			}
		})
	}
}

func TestSymbolStatsExcludeForeignLog(t *testing.T) {
	s := NewGOBSymbolStore(filepath.Join(t.TempDir(), "symbols.gob"))
	saveReviewFile(t, s, "base.go")
	if err := os.WriteFile(s.logPath, symbolLogHeader("other"), 0600); err != nil {
		t.Fatal(err)
	}
	stats, err := s.GetStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.IndexSize != s.baseStamp.size {
		t.Fatalf("counted foreign log: %d", stats.IndexSize)
	}
	if err := os.WriteFile(s.logPath, symbolLogHeader(s.logID), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.indexPath, []byte("new base"), 0600); err != nil {
		t.Fatal(err)
	}
	stats, err = s.GetStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.IndexSize != int64(len("new base")) {
		t.Fatal("counted old log after base replacement")
	}
}

func TestSymbolOrderStableAcrossMerge(t *testing.T) {
	ctx := context.Background()
	s := NewGOBSymbolStore(filepath.Join(t.TempDir(), "symbols.gob"))
	saveReviewFile(t, s, "z.go")
	saveReviewFile(t, s, "a.go")
	for i := 0; i < 2; i++ {
		syms, err := s.LookupSymbol(ctx, "Run")
		if err != nil {
			t.Fatal(err)
		}
		if len(syms) != 2 || syms[0].File != "a.go" {
			t.Fatalf("bad order: %+v", syms)
		}
		graph, err := s.GetCallGraph(ctx, "Run", 1)
		if err != nil {
			t.Fatal(err)
		}
		if graph.Nodes["Run"].File != "a.go" {
			t.Fatal("graph root changed")
		}
		if err := s.compactUnlocked(time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSymbolFailedMergeReloadKeepsPending(t *testing.T) {
	ctx := context.Background()
	s := NewGOBSymbolStore(filepath.Join(t.TempDir(), "symbols.gob"))
	saveReviewFile(t, s, "base.go")
	saveReviewFile(t, s, "log.go")
	if err := s.SaveFile(ctx, "pending.go", []Symbol{{Name: "Pending", File: "pending.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(s.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.indexPath, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Persist(ctx); err == nil {
		t.Fatal("bad base accepted")
	}
	if !s.IsFileIndexed("pending.go") {
		t.Fatal("pending edit lost")
	}
	if err := os.WriteFile(s.indexPath, good, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Persist(ctx); err != nil {
		t.Fatal(err)
	}
	check := NewGOBSymbolStore(s.indexPath)
	if err := check.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !check.IsFileIndexed("pending.go") {
		t.Fatal("retry lost pending edit")
	}
}
