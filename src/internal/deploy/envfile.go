package deploy

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GetVar returns the value of key in a KEY=VALUE file; ok is false when the
// file or the key is missing. Comments and blank lines are ignored.
func GetVar(path, key string) (value string, ok bool, err error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, found := parseLine(sc.Text())
		if found && k == key {
			value, ok = v, true // last assignment wins, as in Compose
		}
	}
	return value, ok, sc.Err()
}

// SetVar sets key=value, keeping every other line as it is. The file is
// created when missing; the write is atomic and keeps the file mode.
func SetVar(path, key, value string) error {
	if value == "" || strings.ContainsAny(value, "\n\r\"' $`\\") {
		return fmt.Errorf("refusing to write unsafe value for %s", key)
	}
	var in []string
	mode := os.FileMode(0o644)
	if data, err := os.ReadFile(path); err == nil {
		in = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	out := make([]string, 0, len(in)+1)
	set := false
	for _, l := range in {
		if k, _, found := parseLine(l); found && k == key {
			if !set {
				out = append(out, key+"="+value)
				set = true
			}
			continue // later duplicates would override the new value
		}
		out = append(out, l)
	}
	if !set {
		out = append(out, key+"="+value)
	}
	return writeAtomic(path, []byte(strings.Join(out, "\n")+"\n"), mode)
}

// UnsetVar removes every assignment of key. A missing file is not an error.
func UnsetVar(path, key string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if k, _, found := parseLine(l); found && k == key {
			continue
		}
		out = append(out, l)
	}
	text := strings.Join(out, "\n")
	if text != "" {
		text += "\n"
	}
	return writeAtomic(path, []byte(text), mode)
}

func parseLine(l string) (key, value string, ok bool) {
	l = strings.TrimSpace(l)
	if l == "" || strings.HasPrefix(l, "#") {
		return "", "", false
	}
	l = strings.TrimPrefix(l, "export ")
	k, v, found := strings.Cut(l, "=")
	if !found {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return strings.TrimSpace(k), v, true
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
