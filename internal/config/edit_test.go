package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveServer_EditsOnlyTheEntry(t *testing.T) {
	tests := []struct {
		name   string
		remove string
		before string
		want   string
	}{
		{
			name:   "middle entry keeps separators and the next head comment",
			remove: "b",
			before: "servers:\n  a:\n    command: x\n\n  # about b\n  b:\n    command: ${B_TOKEN}\n    # inside b\n    port: 2\n\n  # about c\n  c:\n    command: z\n",
			want:   "servers:\n  a:\n    command: x\n\n  # about c\n  c:\n    command: z\n",
		},
		{
			name:   "last entry before another section",
			remove: "b",
			before: "# top\nservers:\n  a:\n    command: x\n  b:\n    command: y\n\n# security settings\nsecurity:\n  bearer_token: ${TOKEN}\n",
			want:   "# top\nservers:\n  a:\n    command: x\n\n# security settings\nsecurity:\n  bearer_token: ${TOKEN}\n",
		},
		{
			name:   "flow value on one line",
			remove: "a",
			before: "servers:\n  a: {command: x, args: [\"-y\"]}\n  b: {command: y}\n",
			want:   "servers:\n  b: {command: y}\n",
		},
		{
			name:   "only entry",
			remove: "a",
			before: "servers:\n  a:\n    command: x\nsupervision:\n  restart_delay: 1s\n",
			want:   "servers:\nsupervision:\n  restart_delay: 1s\n",
		},
		{
			name:   "CRLF line endings",
			remove: "a",
			before: "servers:\r\n  a:\r\n    command: x\r\n  b:\r\n    command: y\r\n",
			want:   "servers:\r\n  b:\r\n    command: y\r\n",
		},
		{
			name:   "later document and end marker stay",
			remove: "b",
			before: "servers:\n  a:\n    command: x\n  b:\n    command: y\n...\n---\nother: doc\n",
			want:   "servers:\n  a:\n    command: x\n...\n---\nother: doc\n",
		},
		{
			name:   "file without trailing newline",
			remove: "b",
			before: "servers:\n  a:\n    command: x\n  b:\n    command: y",
			want:   "servers:\n  a:\n    command: x\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "servers.yaml")
			if err := os.WriteFile(path, []byte(tt.before), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := RemoveServer(path, tt.remove); err != nil {
				t.Fatalf("RemoveServer() error = %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestRemoveServer_LeavesFileUntouched(t *testing.T) {
	tests := []struct {
		name    string
		remove  string
		content string
		wantErr error
	}{
		{name: "absent entry", remove: "missing", content: "servers:\n  a:\n    command: x\n"},
		{name: "no servers key", remove: "a", content: "security: {}\n"},
		{name: "absent entry in flow servers mapping", remove: "missing", content: "servers: {a: {command: x}}\n"},
		{name: "flow servers mapping", remove: "a", content: "servers: {a: {command: x}, b: {command: y}}\n", wantErr: ErrUnsupportedEdit},
		{name: "entry anchors an alias used elsewhere", remove: "a", content: "servers:\n  a: &shared\n    command: x\n  b: *shared\n", wantErr: ErrUnsupportedEdit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "servers.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			err := RemoveServer(path, tt.remove)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("RemoveServer() error = %v, want %v", err, tt.wantErr)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.content {
				t.Fatalf("file changed:\n%q\nwant:\n%q", got, tt.content)
			}
		})
	}
}

func TestRemoveServer_MissingFileIsNoOp(t *testing.T) {
	if err := RemoveServer(filepath.Join(t.TempDir(), "servers.yaml"), "a"); err != nil {
		t.Fatalf("RemoveServer() error = %v", err)
	}
}

func TestRemoveServer_EditsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "servers.yaml")
	if err := os.WriteFile(target, []byte("servers:\n  a:\n    command: x\n  b:\n    command: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveServer(link, "a"); err != nil {
		t.Fatalf("RemoveServer() error = %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: mode=%v err=%v", fi.Mode(), err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "servers:\n  b:\n    command: y\n" {
		t.Fatalf("target = %q", got)
	}
}
