// Package attachments stores opaque, content-addressed local image files.
package attachments

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const MaxImageSize = 20 << 20

type StoredFile struct {
	Hash string
	MIME string
	Size int64
}

type Store struct{ root string }

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create attachment directory: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) Save(reader io.Reader) (StoredFile, error) {
	temporary, err := os.CreateTemp(s.root, ".upload-*")
	if err != nil {
		return StoredFile{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	hash := sha256.New()
	limited := io.LimitReader(reader, MaxImageSize+1)
	buffer := make([]byte, 512)
	count, readErr := io.ReadFull(limited, buffer)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		temporary.Close()
		return StoredFile{}, readErr
	}
	buffer = buffer[:count]
	mimeType := http.DetectContentType(buffer)
	if !allowedImageType(mimeType) {
		temporary.Close()
		return StoredFile{}, fmt.Errorf("only JPEG, PNG, WebP, and GIF images are allowed")
	}
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.MultiReader(bytes.NewReader(buffer), limited))
	if err != nil {
		temporary.Close()
		return StoredFile{}, err
	}
	if written == 0 || written > MaxImageSize {
		temporary.Close()
		return StoredFile{}, fmt.Errorf("image must be between 1 byte and 20 MiB")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return StoredFile{}, err
	}
	if err := temporary.Close(); err != nil {
		return StoredFile{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	directory := filepath.Join(s.root, "sha256", digest[:2])
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return StoredFile{}, err
	}
	path := filepath.Join(directory, digest)
	if _, err := os.Stat(path); err == nil {
		return StoredFile{Hash: digest, MIME: mimeType, Size: written}, nil
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return StoredFile{}, err
	}
	return StoredFile{Hash: digest, MIME: mimeType, Size: written}, nil
}

func (s *Store) Open(hash string) (*os.File, error) {
	if len(hash) != sha256.Size*2 {
		return nil, os.ErrNotExist
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return nil, os.ErrNotExist
	}
	return os.Open(filepath.Join(s.root, "sha256", hash[:2], hash))
}

func (s *Store) Delete(hash string) error {
	if len(hash) != sha256.Size*2 {
		return os.ErrNotExist
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return os.ErrNotExist
	}
	err := os.Remove(filepath.Join(s.root, "sha256", hash[:2], hash))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) CopyTo(hash, target string) error {
	source, err := s.Open(hash)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".thoughtglean-image-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := io.Copy(temporary, source); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, target)
}

func allowedImageType(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}
