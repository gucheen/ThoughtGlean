// Package mirror maintains a readable Markdown projection of active notes.
package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"thoughtglean/internal/attachments"
	"thoughtglean/internal/store"
)

type Mirror struct {
	root        string
	attachments *attachments.Store
}

func New(root string, attachmentStore *attachments.Store) (*Mirror, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("Markdown mirror directory is required")
	}
	return &Mirror{root: filepath.Clean(root), attachments: attachmentStore}, nil
}

// Sync writes every active note before removing stale generated files, so a
// failed sync never deletes the only readable copy of an existing note.
func (m *Mirror) Sync(ctx context.Context, noteStore *store.Store) error {
	notes, err := noteStore.ActiveNotes(ctx)
	if err != nil {
		return err
	}
	sources, err := noteStore.ListNoteSources(ctx)
	if err != nil {
		return err
	}
	sourceByNote := make(map[string]store.NoteSource, len(sources))
	for _, source := range sources {
		sourceByNote[source.NoteID] = source
	}
	allAttachments, err := noteStore.ListAllAttachments(ctx)
	if err != nil {
		return err
	}
	attachmentsByNote := make(map[string][]store.Attachment)
	expectedAttachments := make(map[string]bool)
	active := make(map[string]bool, len(notes))
	for _, note := range notes {
		active[note.ID] = true
	}
	for _, attachment := range allAttachments {
		if active[attachment.NoteID] {
			attachmentsByNote[attachment.NoteID] = append(attachmentsByNote[attachment.NoteID], attachment)
			expectedAttachments[attachmentFilename(attachment)] = true
		}
	}
	if m.attachments != nil {
		copied := make(map[string]bool)
		for _, attachment := range allAttachments {
			if !active[attachment.NoteID] || copied[attachment.ContentHash] {
				continue
			}
			copied[attachment.ContentHash] = true
			if err := m.attachments.CopyTo(attachment.ContentHash, filepath.Join(m.root, "attachments", attachmentFilename(attachment))); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return fmt.Errorf("create mirror directory: %w", err)
	}
	expected := make(map[string]bool, len(notes))
	for _, note := range notes {
		name := noteFilename(note.ID)
		expected[name] = true
		if err := writeAtomically(filepath.Join(m.root, name), []byte(renderNote(note, sourceByNote[note.ID], attachmentsByNote[note.ID])), 0o600); err != nil {
			return err
		}
	}
	if err := writeAtomically(filepath.Join(m.root, "index.md"), []byte(renderIndex(notes)), 0o600); err != nil {
		return err
	}
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return fmt.Errorf("read mirror directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "note-") || !strings.HasSuffix(entry.Name(), ".md") || expected[entry.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(m.root, entry.Name())); err != nil {
			return fmt.Errorf("remove stale mirror note: %w", err)
		}
	}
	attachmentDir := filepath.Join(m.root, "attachments")
	attachmentEntries, err := os.ReadDir(attachmentDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read mirror attachment directory: %w", err)
	}
	for _, entry := range attachmentEntries {
		if entry.IsDir() || expectedAttachments[entry.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(attachmentDir, entry.Name())); err != nil {
			return fmt.Errorf("remove stale mirror attachment: %w", err)
		}
	}
	return nil
}

func noteFilename(id string) string { return "note-" + id + ".md" }

func renderNote(note store.Note, source store.NoteSource, attachments []store.Attachment) string {
	title := note.Title
	if title == "" {
		title = "Untitled note"
	}
	var out strings.Builder
	out.WriteString("---\n")
	fmt.Fprintf(&out, "thoughtglean_id: %s\ncreated_at: %q\nupdated_at: %q\nrevision: %d\nstarred: %t\n", note.ID, note.CreatedAt, note.UpdatedAt, note.Revision, note.Starred)
	if note.ContinuedFromID != nil {
		fmt.Fprintf(&out, "continued_from_id: %s\n", *note.ContinuedFromID)
	}
	if source.URL != "" {
		fmt.Fprintf(&out, "source_url: %q\nsource_title: %q\n", source.URL, source.Title)
	}
	out.WriteString("---\n\n# ")
	out.WriteString(title)
	out.WriteString("\n\n")
	if source.URL != "" {
		title := source.Title
		if title == "" {
			title = source.URL
		}
		fmt.Fprintf(&out, "来源：[%s](%s)\n\n", title, source.URL)
	}
	for _, attachment := range attachments {
		fmt.Fprintf(&out, "![%s](attachments/%s)\n\n", attachment.OriginalName, attachmentFilename(attachment))
	}
	out.WriteString(note.Content)
	out.WriteString("\n")
	return out.String()
}

func attachmentFilename(attachment store.Attachment) string {
	ext := ".img"
	switch attachment.MIMEType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}
	return attachment.ContentHash + ext
}

func renderIndex(notes []store.Note) string {
	sorted := append([]store.Note(nil), notes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt > sorted[j].CreatedAt })
	var out strings.Builder
	out.WriteString("# ThoughtGlean Markdown mirror\n\n")
	out.WriteString("This directory is generated by ThoughtGlean. Edit notes in ThoughtGlean; manual changes here will be overwritten.\n\n")
	for _, note := range sorted {
		title := note.Title
		if title == "" {
			title = "Untitled note"
		}
		fmt.Fprintf(&out, "- [%s](%s) — %s\n", title, noteFilename(note.ID), note.CreatedAt)
	}
	return out.String()
}

func writeAtomically(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".thoughtglean-*")
	if err != nil {
		return fmt.Errorf("create mirror temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write mirror file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync mirror file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace mirror file: %w", err)
	}
	return nil
}
