package deploy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	historyFile  = "history.jsonl"
	maxReadBytes = 1 << 20
)

// Entry is one line of .helmo/history.jsonl.
type Entry struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"` // deploy | rollback | auto-rollback
	From   string    `json:"from,omitempty"`
	To     string    `json:"to"`
	Result string    `json:"result"` // ok | failed | rolled_back
	By     string    `json:"by,omitempty"`
	Error  string    `json:"error,omitempty"`
}

func historyPath(appDir string) string { return filepath.Join(appDir, ".helmo", historyFile) }

// Append adds one entry as a single O_APPEND write.
func Append(appDir string, e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(historyPath(appDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Recent returns up to n newest entries, oldest first. Only the last
// megabyte of the file is read, so a long history costs bounded memory.
func Recent(appDir string, n int) ([]Entry, error) {
	f, err := os.Open(historyPath(appDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size, offset := fi.Size(), int64(0)
	if size > maxReadBytes {
		offset, size = size-maxReadBytes, maxReadBytes
	}
	data := make([]byte, size)
	if _, err := f.ReadAt(data, offset); err != nil && err != io.EOF {
		return nil, err
	}
	if offset > 0 { // the first line is probably cut in half
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	var all []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Action != "" {
			all = append(all, e)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, sc.Err()
}

// RollbackTarget picks the version to return to: the version that was active
// before the current one, according to the entries that succeeded.
func RollbackTarget(entries []Entry) (Version, bool) {
	var active []Entry
	for _, e := range entries {
		if e.Result == "ok" {
			active = append(active, e)
		}
	}
	if len(active) == 0 {
		return Version{}, false
	}
	current := active[len(active)-1].To
	for i := len(active) - 1; i >= 0; i-- {
		if active[i].To != current {
			return parseCurrent(active[i].To), true
		}
	}
	// only the current version succeeded so far: go back to what it replaced
	if from := active[0].From; from != "" && from != current {
		return parseCurrent(from), true
	}
	return Version{}, false
}
