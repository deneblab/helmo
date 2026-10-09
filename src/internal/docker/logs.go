package docker

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"time"
)

const (
	maxLineBytes = 64 << 10
	truncMark    = " ...[truncated]"
	readChunk    = 32 << 10
)

// LogLine is one line of container output.
type LogLine struct {
	Time   time.Time // zero when Docker gave no parsable timestamp
	Stream string    // "stdout" or "stderr"
	Text   string
}

// LogOptions selects what StreamLogs returns.
type LogOptions struct {
	Tail   int  // number of most recent lines first
	Follow bool // keep streaming new lines until ctx ends or the container stops
}

// readLogs turns a Docker log stream into lines. Without a TTY the stream is
// multiplexed in frames: 1 byte stream type, 3 zero bytes, 4 bytes big-endian
// payload size, then the payload. Memory is bounded by one chunk plus one
// partial line per stream, never by the amount of history.
func readLogs(r io.Reader, tty bool, emit func(LogLine) error) error {
	if tty {
		s := &splitter{stream: "stdout"}
		if err := s.copyFrom(r, emit); err != nil {
			return err
		}
		return s.flush(emit)
	}

	br := bufio.NewReaderSize(r, readChunk)
	splitters := map[string]*splitter{"stdout": {stream: "stdout"}, "stderr": {stream: "stderr"}}
	buf := make([]byte, readChunk)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		stream := "stdout"
		if hdr[0] == 2 {
			stream = "stderr"
		}
		s := splitters[stream]
		for remaining := int64(binary.BigEndian.Uint32(hdr[4:])); remaining > 0; {
			n := int64(len(buf))
			if remaining < n {
				n = remaining
			}
			if _, err := io.ReadFull(br, buf[:n]); err != nil {
				return err
			}
			if err := s.write(buf[:n], emit); err != nil {
				return err
			}
			remaining -= n
		}
	}
	for _, name := range []string{"stdout", "stderr"} {
		if err := splitters[name].flush(emit); err != nil {
			return err
		}
	}
	return nil
}

// splitter assembles lines from arbitrary chunks, capping line length.
type splitter struct {
	stream    string
	buf       []byte
	truncated bool
}

func (s *splitter) copyFrom(r io.Reader, emit func(LogLine) error) error {
	buf := make([]byte, readChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := s.write(buf[:n], emit); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *splitter) write(p []byte, emit func(LogLine) error) error {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.add(p)
			return nil
		}
		s.add(p[:i])
		if err := s.flush(emit); err != nil {
			return err
		}
		p = p[i+1:]
	}
	return nil
}

func (s *splitter) add(p []byte) {
	room := maxLineBytes - len(s.buf)
	if len(p) > room {
		p, s.truncated = p[:room], true
	}
	s.buf = append(s.buf, p...)
}

func (s *splitter) flush(emit func(LogLine) error) error {
	if len(s.buf) == 0 && !s.truncated {
		return nil
	}
	text := strings.TrimSuffix(string(s.buf), "\r")
	if s.truncated {
		text += truncMark
	}
	s.buf, s.truncated = s.buf[:0], false
	return emit(parseLine(s.stream, text))
}

// parseLine splits the RFC 3339 timestamp Docker prefixes with timestamps=1.
func parseLine(stream, raw string) LogLine {
	if ts, rest, ok := strings.Cut(raw, " "); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return LogLine{Time: t, Stream: stream, Text: rest}
		}
	}
	return LogLine{Stream: stream, Text: raw}
}
