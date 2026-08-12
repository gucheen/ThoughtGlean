package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"thoughtglean/internal/attachments"
	"thoughtglean/internal/store"
)

func TestAutomaticBackupRestoresInIsolationAndPrunes(t *testing.T) {
	root := t.TempDir()
	notes, err := store.Open(filepath.Join(root, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer notes.Close()
	files, err := attachments.New(filepath.Join(root, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	note, _, err := notes.CreateNote(context.Background(), store.CreateNoteInput{Title: "演练记录", Content: "完整正文"})
	if err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := files.Save(bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notes.AddAttachment(context.Background(), store.Attachment{NoteID: note.ID, ContentHash: stored.Hash, OriginalName: "pixel.png", MIMEType: stored.MIME, ByteSize: stored.Size}); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "backups")
	var latest Report
	for range 3 {
		latest, err = WriteAutomatic(context.Background(), notes, files, directory, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	archives, err := filepath.Glob(filepath.Join(directory, "thoughtglean-auto-*.zip"))
	if err != nil || len(archives) != 2 {
		t.Fatalf("archives=%v err=%v", archives, err)
	}
	report, err := RestoreDrill(context.Background(), latest.Path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Notes != 1 || report.Revisions != 1 || report.Attachments != 1 || report.SHA256 == "" {
		t.Fatalf("report=%#v", report)
	}
}

func TestRestoreDrillRejectsCorruptArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.zip")
	if err := os.WriteFile(path, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreDrill(context.Background(), path); err == nil {
		t.Fatal("corrupt backup passed restore drill")
	}
}
