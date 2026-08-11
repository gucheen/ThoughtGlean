package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrSyncVaultNotFound intentionally does not distinguish an absent vault
// from a wrong secret at the HTTP boundary.
var ErrSyncVaultNotFound = errors.New("sync vault not found")

type EncryptedOperation struct {
	Sequence    int64  `json:"sequence"`
	OperationID string `json:"operationId"`
	Ciphertext  string `json:"ciphertext"`
	CreatedAt   string `json:"createdAt"`
}

func syncTokenHash(token []byte) []byte {
	hash := sha256.Sum256(token)
	return hash[:]
}

func validSyncVaultID(id string) bool {
	if len(id) < 22 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

// ClaimSyncVault creates a new opaque vault. A matching claim is idempotent,
// while a different secret never reveals whether the vault already exists.
func (s *Store) ClaimSyncVault(ctx context.Context, vaultID string, token []byte) error {
	if !validSyncVaultID(vaultID) || len(token) != 32 {
		return invalidInput("invalid sync vault credentials")
	}
	hash := syncTokenHash(token)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `INSERT INTO sync_vaults (id, token_hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO NOTHING`, vaultID, hash, now)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	var stored []byte
	if err := s.db.QueryRowContext(ctx, `SELECT token_hash FROM sync_vaults WHERE id = ?`, vaultID).Scan(&stored); err != nil {
		return err
	}
	if len(stored) != len(hash) || !constantTimeEqual(stored, hash) {
		return ErrSyncVaultNotFound
	}
	return nil
}

func (s *Store) hasSyncVault(ctx context.Context, vaultID string, token []byte) (bool, error) {
	if !validSyncVaultID(vaultID) || len(token) != 32 {
		return false, nil
	}
	var stored []byte
	err := s.db.QueryRowContext(ctx, `SELECT token_hash FROM sync_vaults WHERE id = ?`, vaultID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(stored) == sha256.Size && constantTimeEqual(stored, syncTokenHash(token)), nil
}

// HasSyncVault validates a vault capability without exposing vault existence
// at the HTTP boundary. It distinguishes an idempotent join from a new claim.
func (s *Store) HasSyncVault(ctx context.Context, vaultID string, token []byte) (bool, error) {
	return s.hasSyncVault(ctx, vaultID, token)
}

// AppendEncryptedOperations accepts opaque ciphertext only. The relay does
// not decode it, and therefore cannot inspect note contents or merge them.
func (s *Store) AppendEncryptedOperations(ctx context.Context, vaultID string, token []byte, operations []EncryptedOperation) ([]EncryptedOperation, error) {
	ok, err := s.hasSyncVault(ctx, vaultID, token)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSyncVaultNotFound
	}
	if len(operations) == 0 || len(operations) > 100 {
		return nil, invalidInput("sync batch must contain 1 to 100 operations")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	accepted := make([]EncryptedOperation, 0, len(operations))
	for _, operation := range operations {
		operation.OperationID = strings.TrimSpace(operation.OperationID)
		if len(operation.OperationID) < 16 || len(operation.OperationID) > 128 || len(operation.Ciphertext) == 0 || len(operation.Ciphertext) > 10<<20 {
			return nil, invalidInput("invalid encrypted sync operation")
		}
		if operation.CreatedAt == "" {
			operation.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO sync_operations (vault_id, operation_id, ciphertext, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(vault_id, operation_id) DO NOTHING`, vaultID, operation.OperationID, operation.Ciphertext, operation.CreatedAt)
		if err != nil {
			return nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed == 1 {
			if err := tx.QueryRowContext(ctx, `SELECT sequence FROM sync_operations WHERE vault_id = ? AND operation_id = ?`, vaultID, operation.OperationID).Scan(&operation.Sequence); err != nil {
				return nil, err
			}
			accepted = append(accepted, operation)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return accepted, nil
}

func (s *Store) ListEncryptedOperations(ctx context.Context, vaultID string, token []byte, after int64, limit int) ([]EncryptedOperation, error) {
	ok, err := s.hasSyncVault(ctx, vaultID, token)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSyncVaultNotFound
	}
	if after < 0 {
		return nil, invalidInput("invalid sync cursor")
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, operation_id, ciphertext, created_at FROM sync_operations WHERE vault_id = ? AND sequence > ? ORDER BY sequence ASC LIMIT ?`, vaultID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]EncryptedOperation, 0)
	for rows.Next() {
		var operation EncryptedOperation
		if err := rows.Scan(&operation.Sequence, &operation.OperationID, &operation.Ciphertext, &operation.CreatedAt); err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

func constantTimeEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}

func (s *Store) SyncOperationCount(ctx context.Context, vaultID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_operations WHERE vault_id = ?`, vaultID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count sync operations: %w", err)
	}
	return count, nil
}

func (s *Store) PutEncryptedBlob(ctx context.Context, vaultID string, token []byte, blobID string, ciphertext []byte) error {
	ok, err := s.hasSyncVault(ctx, vaultID, token)
	if err != nil {
		return err
	}
	if !ok {
		return ErrSyncVaultNotFound
	}
	if !validSyncVaultID(blobID) || len(ciphertext) == 0 || len(ciphertext) > 30<<20 {
		return invalidInput("invalid encrypted sync blob")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO sync_blobs (vault_id, blob_id, ciphertext, created_at) VALUES (?, ?, ?, ?) ON CONFLICT(vault_id,blob_id) DO NOTHING`, vaultID, blobID, ciphertext, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetEncryptedBlob(ctx context.Context, vaultID string, token []byte, blobID string) ([]byte, error) {
	ok, err := s.hasSyncVault(ctx, vaultID, token)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSyncVaultNotFound
	}
	var ciphertext []byte
	err = s.db.QueryRowContext(ctx, `SELECT ciphertext FROM sync_blobs WHERE vault_id=? AND blob_id=?`, vaultID, blobID).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ciphertext, err
}
