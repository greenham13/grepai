package trace

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"time"
)

// The change log extends one base snapshot. Layout:
//
//	header: symbolLogMagic, uint16 id length, id bytes (the base's LogID)
//	frames: uint32 payload length, uint32 CRC-32C of payload, payload
//
// Each payload is one gob-encoded symbolLogBatch, written by one Persist.
// A record holds the complete state of one file, so replaying a record
// twice, or on a base that already contains it, gives the same result.
// That makes compaction crash-safe: the new base is renamed into place
// first, and a log left behind names the old base and is never replayed.
// A torn final frame (crash mid-append) fails its length or CRC check;
// replay stops there and the next append truncates it.

const symbolLogMagic = "GREPAI-SYMLOG\x01"

var (
	// The log is folded into a new base once it outgrows both limits.
	symbolLogCompactMinBytes int64 = 16 << 20
	symbolLogCompactDivisor  int64 = 4

	symbolLogCRC = crc32.MakeTable(crc32.Castagnoli)
)

type symbolLogBatch struct {
	UpdatedAt time.Time
	Records   []symbolLogRecord
}

type symbolLogRecord struct {
	File       string
	MetaOnly   bool // only the metadata below changed; keep the entries
	Indexed    bool
	Hash       string
	HasHash    bool
	Version    string
	HasVersion bool
	Symbols    []Symbol
	Refs       []Reference
	Edges      []CallEdge
}

func symbolLogHeader(id string) []byte {
	header := make([]byte, 0, len(symbolLogMagic)+2+len(id))
	header = append(header, symbolLogMagic...)
	header = binary.LittleEndian.AppendUint16(header, uint16(len(id)))
	return append(header, id...)
}

func (s *GOBSymbolStore) encodeDirtyBatchUnlocked(now time.Time) ([]byte, error) {
	files := make([]string, 0, len(s.dirty))
	for file := range s.dirty {
		files = append(files, file)
	}
	sort.Strings(files)
	batch := symbolLogBatch{UpdatedAt: now, Records: make([]symbolLogRecord, 0, len(files))}
	for _, file := range files {
		record := symbolLogRecord{File: file, MetaOnly: !s.dirty[file], Indexed: s.fileIndex[file]}
		record.Hash, record.HasHash = s.fileContentHashes[file]
		record.Version, record.HasVersion = s.fileExtractorVersions[file]
		if !record.MetaOnly {
			for _, name := range s.fileSymNames[file] {
				record.Symbols = append(record.Symbols, s.symbols[name][file]...)
			}
			for _, name := range s.fileRefNames[file] {
				record.Refs = append(record.Refs, s.refs[name][file]...)
			}
			record.Edges = s.edges[file]
		}
		batch.Records = append(batch.Records, record)
	}
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(batch); err != nil {
		return nil, fmt.Errorf("failed to encode symbol log batch: %w", err)
	}
	frame := make([]byte, 8, 8+payload.Len())
	binary.LittleEndian.PutUint32(frame[0:4], uint32(payload.Len()))
	binary.LittleEndian.PutUint32(frame[4:8], crc32.Checksum(payload.Bytes(), symbolLogCRC))
	return append(frame, payload.Bytes()...), nil
}

func (s *GOBSymbolStore) logTooLargeUnlocked(frameSize int64) bool {
	limit := s.baseStamp.size / symbolLogCompactDivisor
	if limit < symbolLogCompactMinBytes {
		limit = symbolLogCompactMinBytes
	}
	size := s.logSize
	if size == 0 {
		size = int64(len(symbolLogHeader(s.logID)))
	}
	return size+frameSize > limit
}

func (s *GOBSymbolStore) appendLogUnlocked(frame []byte) error {
	file, err := os.OpenFile(s.logPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open symbol log: %w", err)
	}
	offset := s.logSize
	data := frame
	if offset == 0 {
		data = append(symbolLogHeader(s.logID), frame...)
	}
	// Drop anything past the valid prefix: a torn frame from a crash, or a
	// log that belonged to another base.
	if err := file.Truncate(offset); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to truncate symbol log: %w", err)
	}
	if _, err := file.WriteAt(data, offset); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to append symbol log: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to sync symbol log: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close symbol log: %w", err)
	}
	s.logSize = offset + int64(len(data))
	return nil
}

// replayLogUnlocked applies the log that extends the loaded base. A missing
// log or one written for another base is ignored; logSize stays 0 so the
// next append starts a fresh log.
func (s *GOBSymbolStore) replayLogUnlocked() error {
	file, err := os.Open(s.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to open symbol log: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat symbol log: %w", err)
	}
	reader := bufio.NewReaderSize(file, 1<<20)

	header := symbolLogHeader(s.logID)
	got := make([]byte, len(header))
	if _, err := io.ReadFull(reader, got); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return fmt.Errorf("failed to read symbol log: %w", err)
	}
	if !bytes.Equal(got, header) {
		return nil
	}
	offset := int64(len(header))
	var prefix [8]byte
	for {
		if _, err := io.ReadFull(reader, prefix[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return fmt.Errorf("failed to read symbol log: %w", err)
		}
		size := int64(binary.LittleEndian.Uint32(prefix[0:4]))
		if offset+8+size > info.Size() {
			break // torn final frame
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return fmt.Errorf("failed to read symbol log: %w", err)
		}
		if crc32.Checksum(payload, symbolLogCRC) != binary.LittleEndian.Uint32(prefix[4:8]) {
			break // torn final frame
		}
		var batch symbolLogBatch
		if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&batch); err != nil {
			// The checksum matched, so this is not a torn write.
			return fmt.Errorf("failed to decode symbol log at offset %d: %w", offset, err)
		}
		for i := range batch.Records {
			s.applyLogRecordUnlocked(&batch.Records[i])
		}
		s.updatedAt = batch.UpdatedAt
		offset += 8 + size
	}
	s.logSize = offset
	return nil
}

func (s *GOBSymbolStore) applyLogRecordUnlocked(record *symbolLogRecord) {
	if !record.MetaOnly {
		s.removeEntriesUnlocked(record.File)
		for _, sym := range record.Symbols {
			s.appendSymbolUnlocked(sym.Name, sym)
		}
		for _, ref := range record.Refs {
			s.appendRefUnlocked(ref.SymbolName, ref)
		}
		if len(record.Edges) > 0 {
			s.edges[record.File] = append([]CallEdge(nil), record.Edges...)
		}
		s.invalidateEdgeCache()
	}
	if record.Indexed {
		s.fileIndex[record.File] = true
	} else {
		delete(s.fileIndex, record.File)
	}
	if record.HasHash {
		s.fileContentHashes[record.File] = record.Hash
	} else {
		delete(s.fileContentHashes, record.File)
	}
	if record.HasVersion {
		s.fileExtractorVersions[record.File] = record.Version
	} else {
		delete(s.fileExtractorVersions, record.File)
	}
}
