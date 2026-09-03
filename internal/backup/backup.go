// Package backup creates durable full backups and verifies them by restoring
// into an isolated temporary library.
package backup

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"thoughtglean/internal/attachments"
	"thoughtglean/internal/store"
)

const maxArchiveSize = 1 << 30

type Report struct {
	Path        string `json:"path"`
	GeneratedAt string `json:"generatedAt"`
	Notes       int    `json:"notes"`
	Revisions   int    `json:"revisions"`
	Sources     int    `json:"sources"`
	Attachments int    `json:"attachments"`
	SHA256      string `json:"sha256"`
}

func WriteFull(ctx context.Context, notes *store.Store, files *attachments.Store, writer io.Writer) (Report, error) {
	snapshot, err := notes.FullBackupData(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("snapshot database: %w", err)
	}
	archive := zip.NewWriter(writer)
	fail := func(cause error) (Report, error) {
		_ = archive.Close()
		return Report{}, cause
	}
	manifest, err := archive.Create("backup.json")
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(manifest).Encode(snapshot); err != nil {
		return fail(err)
	}
	seen := make(map[string]bool)
	for _, item := range snapshot.Attachments {
		if seen[item.ContentHash] {
			continue
		}
		seen[item.ContentHash] = true
		file, err := files.Open(item.ContentHash)
		if err != nil {
			return fail(fmt.Errorf("open attachment %s: %w", item.ContentHash, err))
		}
		header := &zip.FileHeader{Name: "attachments/" + item.ContentHash, Method: zip.Store}
		header.SetMode(0o600)
		entry, createErr := archive.CreateHeader(header)
		if createErr == nil {
			_, createErr = io.Copy(entry, file)
		}
		file.Close()
		if createErr != nil {
			return fail(fmt.Errorf("archive attachment %s: %w", item.ContentHash, createErr))
		}
	}
	if err := archive.Close(); err != nil {
		return Report{}, err
	}
	return reportFor(snapshot), nil
}

func WriteAutomatic(ctx context.Context, notes *store.Store, files *attachments.Store, directory string, keep int) (Report, error) {
	if keep < 1 {
		return Report{}, errors.New("backup retention must be at least 1")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Report{}, fmt.Errorf("create backup directory: %w", err)
	}
	name := "thoughtglean-auto-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".zip"
	temporary, err := os.CreateTemp(directory, ".thoughtglean-backup-*.tmp")
	if err != nil {
		return Report{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	_ = temporary.Chmod(0o600)
	report, err := WriteFull(ctx, notes, files, temporary)
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Report{}, err
	}
	target := filepath.Join(directory, name)
	if err := os.Rename(temporaryName, target); err != nil {
		return Report{}, err
	}
	report.Path = target
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	if err := prune(directory, keep); err != nil {
		return report, err
	}
	return report, nil
}

func prune(directory string, keep int) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "thoughtglean-auto-") && strings.HasSuffix(entry.Name(), ".zip") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names[:max(0, len(names)-keep)] {
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return nil
}

func RestoreDrill(ctx context.Context, archivePath string) (Report, error) {
	root, err := os.MkdirTemp("", "thoughtglean-restore-drill-*")
	if err != nil {
		return Report{}, err
	}
	defer os.RemoveAll(root)
	snapshot, err := readArchive(archivePath, filepath.Join(root, "attachments"))
	if err != nil {
		return Report{}, err
	}
	restored, err := store.Open(filepath.Join(root, "thoughtglean.db"))
	if err != nil {
		return Report{}, err
	}
	defer restored.Close()
	if err := restored.RestoreBackup(ctx, snapshot); err != nil {
		return Report{}, fmt.Errorf("restore isolated database: %w", err)
	}
	verification, err := restored.FullBackupData(ctx)
	if err != nil {
		return Report{}, err
	}
	if !equalJSON(snapshot.Notes, verification.Notes) || !equalJSON(snapshot.Revisions, verification.Revisions) ||
		!equalJSON(snapshot.Sources, verification.Sources) || !equalJSON(snapshot.MaterialLinks, verification.MaterialLinks) ||
		!equalJSON(snapshot.Verifications, verification.Verifications) || !equalJSON(snapshot.Topics, verification.Topics) ||
		!equalJSON(snapshot.TopicMemberships, verification.TopicMemberships) || !equalJSON(snapshot.Attachments, verification.Attachments) ||
		!equalJSON(snapshot.Requests, verification.Requests) {
		return Report{}, errors.New("restored data does not match backup")
	}
	report := reportFor(snapshot)
	report.Path = archivePath
	return report, nil
}

func readArchive(path, attachmentRoot string) (store.Backup, error) {
	info, err := os.Stat(path)
	if err != nil {
		return store.Backup{}, err
	}
	if info.Size() > maxArchiveSize {
		return store.Backup{}, errors.New("backup exceeds 1 GiB")
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		return store.Backup{}, fmt.Errorf("open backup ZIP: %w", err)
	}
	defer archive.Close()
	entries := make(map[string]*zip.File, len(archive.File))
	var totalSize uint64
	for _, entry := range archive.File {
		totalSize += entry.UncompressedSize64
		if entry.UncompressedSize64 > maxArchiveSize || totalSize > maxArchiveSize || entry.Name == "" || filepath.IsAbs(entry.Name) || strings.Contains(entry.Name, "..") {
			return store.Backup{}, errors.New("backup contains an unsafe entry")
		}
		if entries[entry.Name] != nil {
			return store.Backup{}, errors.New("backup contains duplicate entries")
		}
		entries[entry.Name] = entry
	}
	manifest := entries["backup.json"]
	if manifest == nil {
		return store.Backup{}, errors.New("backup.json is missing")
	}
	reader, err := manifest.Open()
	if err != nil {
		return store.Backup{}, err
	}
	var snapshot store.Backup
	err = json.NewDecoder(io.LimitReader(reader, 64<<20)).Decode(&snapshot)
	reader.Close()
	if err != nil {
		return store.Backup{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if err := store.VerifyBackup(snapshot); err != nil {
		return store.Backup{}, err
	}
	files, err := attachments.New(attachmentRoot)
	if err != nil {
		return store.Backup{}, err
	}
	verified := make(map[string]attachments.StoredFile)
	for _, item := range snapshot.Attachments {
		if stored, exists := verified[item.ContentHash]; exists {
			if stored.Size != item.ByteSize || stored.MIME != item.MIMEType {
				return store.Backup{}, fmt.Errorf("attachment %s has inconsistent metadata", item.ContentHash)
			}
			continue
		}
		entry := entries["attachments/"+item.ContentHash]
		if entry == nil {
			return store.Backup{}, fmt.Errorf("attachment %s is missing", item.ContentHash)
		}
		image, err := entry.Open()
		if err != nil {
			return store.Backup{}, err
		}
		stored, saveErr := files.Save(image)
		image.Close()
		if saveErr != nil || stored.Hash != item.ContentHash || stored.Size != item.ByteSize || stored.MIME != item.MIMEType {
			return store.Backup{}, fmt.Errorf("attachment %s failed integrity verification", item.ContentHash)
		}
		verified[item.ContentHash] = stored
	}
	return snapshot, nil
}

func reportFor(snapshot store.Backup) Report {
	return Report{GeneratedAt: snapshot.GeneratedAt, Notes: len(snapshot.Notes), Revisions: len(snapshot.Revisions), Sources: len(snapshot.Sources), Attachments: len(snapshot.Attachments), SHA256: snapshot.Integrity.SHA256}
}

func equalJSON[T any](left, right []T) bool {
	if len(left) != len(right) {
		return false
	}
	if len(left) == 0 {
		return true
	}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
