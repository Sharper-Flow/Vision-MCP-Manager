package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrUnsupportedEdit reports a servers.yaml layout that RemoveServer cannot
// edit without rewriting text it does not own.
var ErrUnsupportedEdit = errors.New("config: unsupported layout for in-place edit")

// RemoveServer deletes the servers.<name> entry from the config file at path
// by removing its source lines. Every other byte of the file stays as written:
// ${VAR} references stay unresolved, and other entries, comments and
// formatting are unchanged. The removed entry's own head comment and trailing
// blank lines go with it; comments that precede the next entry stay.
//
// A missing file or entry is not an error. A flow-style servers mapping, or an
// edit whose result does not parse to the original minus the entry, returns
// an error and leaves the file untouched.
func RemoveServer(path, name string) error {
	realPath, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: resolve path: %w", err)
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return fmt.Errorf("config: stat: %w", err)
	}
	data, err := os.ReadFile(realPath)
	if err != nil {
		return fmt.Errorf("config: read error: %w", err)
	}

	out, changed, err := removeServerLines(data, name)
	if err != nil || !changed {
		return err
	}
	if err := verifyServerRemoved(data, out, name); err != nil {
		return err
	}
	return writeFileAtomic(realPath, out, info.Mode().Perm())
}

func removeServerLines(data []byte, name string) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrConfigParse, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("%w: top level is not a mapping", ErrUnsupportedEdit)
	}

	serversAt := -1
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "servers" {
			serversAt = i
			break
		}
	}
	if serversAt < 0 {
		return nil, false, nil
	}
	servers := root.Content[serversAt+1]
	if servers.Kind != yaml.MappingNode {
		return nil, false, nil
	}
	if servers.Style&yaml.FlowStyle != 0 {
		return nil, false, fmt.Errorf("%w: servers is a flow mapping", ErrUnsupportedEdit)
	}

	entryAt := -1
	for i := 0; i+1 < len(servers.Content); i += 2 {
		if servers.Content[i].Value == name {
			entryAt = i
			break
		}
	}
	if entryAt < 0 {
		return nil, false, nil
	}

	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	key := servers.Content[entryAt]
	keyIdx := key.Line - 1
	end := len(lines)
	switch {
	case entryAt+2 < len(servers.Content):
		end = servers.Content[entryAt+2].Line - 1
	case serversAt+2 < len(root.Content):
		end = root.Content[serversAt+2].Line - 1
	}
	if end <= keyIdx {
		return nil, false, fmt.Errorf("%w: entry %q shares a line with the next key", ErrUnsupportedEdit, name)
	}
	indent := key.Column - 1

	// The entry's head comment: comment lines directly above it at its indent.
	start := keyIdx
	for start > 0 && isCommentLine(lines[start-1]) && lineIndent(lines[start-1]) == indent {
		start--
	}

	// Trailing trivia: blank lines and comments no deeper than the key. From
	// the first such comment on, the lines belong to whatever follows. Blank
	// lines before that comment go with the entry only when the entry was
	// already separated from what precedes it; otherwise they are the one
	// separator left between the previous content and what follows.
	trivia := end
	for trivia > keyIdx+1 && (isBlankLine(lines[trivia-1]) ||
		(isCommentLine(lines[trivia-1]) && lineIndent(lines[trivia-1]) <= indent)) {
		trivia--
	}
	keepFrom := end
	for i := trivia; i < end; i++ {
		if isCommentLine(lines[i]) {
			keepFrom = i
			break
		}
	}
	if entryAt > 0 && !isBlankLine(lines[start-1]) {
		keepFrom = trivia
	}

	var b strings.Builder
	for _, line := range lines[:start] {
		b.WriteString(line)
	}
	for _, line := range lines[keepFrom:] {
		b.WriteString(line)
	}
	return []byte(b.String()), true, nil
}

// verifyServerRemoved proves the text edit removed exactly one entry: the
// result must decode to the original document minus servers.<name>.
func verifyServerRemoved(before, after []byte, name string) error {
	var want, got map[string]any
	if err := yaml.Unmarshal(before, &want); err != nil {
		return fmt.Errorf("%w: %v", ErrConfigParse, err)
	}
	if err := yaml.Unmarshal(after, &got); err != nil {
		return fmt.Errorf("%w: edited file does not parse: %v", ErrUnsupportedEdit, err)
	}
	servers, _ := want["servers"].(map[string]any)
	delete(servers, name)
	if len(servers) == 0 {
		delete(want, "servers")
	}
	if gotServers, _ := got["servers"].(map[string]any); len(gotServers) == 0 {
		delete(got, "servers")
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("%w: edit changed more than server %q", ErrUnsupportedEdit, name)
	}
	return nil
}

func isBlankLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

func isCommentLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

func lineIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}
