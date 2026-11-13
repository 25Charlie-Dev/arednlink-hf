package app

import (
	"context"

	"os"
	"path/filepath"

	"strings"
	"testing"
)

func TestEditorValidationAndAtomicSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := NewConfigEditor(Defaults(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, e)
	ctx := context.Background()
	for _, bad := range []string{`{"max_peers":2,"max_peers":3}`, `{"unknown":true}`, `{"peer_table":{"full_policy":"drop"}}`, `{"enable_probes":true}`, `{"radio":{"kind":"unavailable"}}`, strings.Repeat(" ", maxConfigBytes+1)} {
		if err := e.Save(ctx, bad); err == nil {
			t.Fatalf("accepted invalid draft %q", bad[:min(len(bad), 50)])
		}
		data, err := readTestFile(path)
		if err != nil || string(data) != "{}" {
			t.Fatalf("invalid draft changed file: %q %v", data, err)
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Save(ctx, `{"max_peers":12}`); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		t.Fatal("save did not atomically replace with mode 0600")
	}
	snapshot, err := e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Pending || !strings.Contains(snapshot.Running, `"max_peers": 64`) || !strings.Contains(snapshot.Saved, `"max_peers": 12`) {
		t.Fatalf("incorrect active/pending state: %+v", snapshot)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.Save(cancelled, `{}`); err == nil {
		t.Fatal("cancelled save accepted")
	}
	data, err := readTestFile(path)
	if err != nil || string(data) != `{"max_peers":12}` {
		t.Fatal("cancelled save changed file")
	}
}

func TestEditorUnsafeTargets(t *testing.T) {
	for _, kind := range []string{"symlink", "writable-directory", "writable-file", "missing"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if kind != "missing" {
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "symlink":
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("config.json.real", path); err != nil {
					t.Fatal(err)
				}
			case "writable-directory":
				setMode(t, dir, 0777)
			case "writable-file":
				setMode(t, path, 0666)
			}
			e, err := NewConfigEditor(Defaults(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(t, e)
			if err := e.Save(context.Background(), `{}`); err == nil {
				t.Fatal("unsafe target saved")
			}
		})
	}
}

func TestEditorRejectsTargetSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := NewConfigEditor(Defaults(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, e)
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("config.json.real", path); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(context.Background(), `{"max_peers":12}`); err == nil {
		t.Fatal("swapped symlink accepted")
	}
	data, err := readTestFile(path + ".real")
	if err != nil || string(data) != "{}" {
		t.Fatal("original modified")
	}
}

func TestEditorIOFailurePreservesFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires unprivileged runtime")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := NewConfigEditor(Defaults(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, e)
	setMode(t, dir, 0500)
	defer setMode(t, dir, 0700)
	if err := e.Save(context.Background(), `{"max_peers":12}`); err == nil {
		t.Fatal("read-only directory save succeeded")
	}
	data, err := readTestFile(path)
	if err != nil || string(data) != "{}" {
		t.Fatal("failed I/O changed file")
	}
}

func TestEditorNoConfigStillValidates(t *testing.T) {
	e, err := NewConfigEditor(Defaults(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, e)
	if err := e.Validate(context.Background(), `{}`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := e.Snapshot(context.Background())
	if err != nil || snapshot.Disabled == "" {
		t.Fatal("missing disabled reason")
	}
	if err := e.Save(context.Background(), `{}`); err == nil {
		t.Fatal("saved without configured path")
	}
}
