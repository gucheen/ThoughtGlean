package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thoughtglean/internal/store"
)

func TestSyncWritesReadableActiveNotesAndRemovesDeletedOnes(t *testing.T) {
	ctx := context.Background()
	noteStore, err := store.Open(filepath.Join(t.TempDir(), "thoughtglean.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { noteStore.Close() })
	active, _, err := noteStore.CreateNote(ctx, store.CreateNoteInput{Title: "留下", Content: "可直接读到的原话"})
	if err != nil {
		t.Fatal(err)
	}
	trashed, _, err := noteStore.CreateNote(ctx, store.CreateNoteInput{Title: "移走", Content: "不在镜像中"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noteStore.DeleteNote(ctx, trashed.ID); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "mirror")
	m, err := New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx, noteStore); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, noteFilename(active.ID)))
	if err != nil || !strings.Contains(string(data), "# 留下") || !strings.Contains(string(data), "可直接读到的原话") {
		t.Fatalf("note mirror=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, noteFilename(trashed.ID))); !os.IsNotExist(err) {
		t.Fatalf("trashed note remains in mirror: %v", err)
	}
	if _, err := noteStore.DeleteNote(ctx, active.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx, noteStore); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, noteFilename(active.ID))); !os.IsNotExist(err) {
		t.Fatalf("stale note remains in mirror: %v", err)
	}
}
