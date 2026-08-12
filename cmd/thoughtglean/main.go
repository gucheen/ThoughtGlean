package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"thoughtglean/internal/app"
	"thoughtglean/internal/attachments"
	"thoughtglean/internal/backup"
	"thoughtglean/internal/store"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "restore-drill" {
		if len(os.Args) != 3 {
			log.Fatal("usage: thoughtglean restore-drill BACKUP.zip")
		}
		report, err := backup.RestoreDrill(context.Background(), os.Args[2])
		if err != nil {
			log.Fatalf("restore drill failed: %v", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			log.Fatal(err)
		}
		return
	}
	addr := envOr("THOUGHTGLEAN_ADDR", "127.0.0.1:8080")
	dataDir := envOr("THOUGHTGLEAN_DATA_DIR", "./data")
	noteStore, err := store.Open(filepath.Join(dataDir, "thoughtglean.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer noteStore.Close()
	attachmentStore, err := attachments.New(filepath.Join(dataDir, "attachments"))
	if err != nil {
		log.Fatal(err)
	}
	backupKeep, err := strconv.Atoi(envOr("THOUGHTGLEAN_BACKUP_KEEP", "14"))
	if err != nil || backupKeep < 1 {
		log.Fatal("THOUGHTGLEAN_BACKUP_KEEP must be a positive integer")
	}
	backupDirectory := envOr("THOUGHTGLEAN_BACKUP_DIR", filepath.Join(dataDir, "backups"))
	if len(os.Args) > 1 && os.Args[1] == "backup-now" {
		report, err := backup.WriteAutomatic(context.Background(), noteStore, attachmentStore, backupDirectory, backupKeep)
		if err != nil {
			log.Fatalf("backup failed: %v", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			log.Fatal(err)
		}
		return
	}
	application := app.New(noteStore)
	application.SetAttachmentStore(attachmentStore)
	application.SetVersion(version)
	ownerToken := os.Getenv("THOUGHTGLEAN_OWNER_TOKEN")
	if ownerToken == "" {
		log.Fatal("THOUGHTGLEAN_OWNER_TOKEN is required")
	}
	if len(ownerToken) < 32 {
		log.Fatal("THOUGHTGLEAN_OWNER_TOKEN must contain at least 32 characters")
	}
	application.SetOwnerToken(ownerToken)
	passkeyRPID := envOr("THOUGHTGLEAN_PASSKEY_RP_ID", "localhost")
	passkeyOrigin := envOr("THOUGHTGLEAN_PASSKEY_ORIGIN", "http://localhost:5173")
	if err := application.ConfigurePasskey(passkeyRPID, passkeyOrigin); err != nil {
		log.Fatalf("configure Passkey: %v", err)
	}
	log.Printf("ThoughtGlean is running as a single-owner note server")
	log.Printf("Passkey RP ID %q, origin %q", passkeyRPID, passkeyOrigin)

	server := &http.Server{
		Addr:              addr,
		Handler:           application,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backupIntervalValue := envOr("THOUGHTGLEAN_BACKUP_INTERVAL", "24h")
	if backupIntervalValue != "0" {
		backupInterval, err := time.ParseDuration(backupIntervalValue)
		if err != nil || backupInterval < time.Minute {
			log.Fatal("THOUGHTGLEAN_BACKUP_INTERVAL must be 0 or a duration of at least 1m")
		}
		go automaticBackups(shutdownContext, noteStore, attachmentStore, backupDirectory, backupKeep, backupInterval)
		log.Printf("Automatic backups every %s; keeping %d in %s", backupInterval, backupKeep, backupDirectory)
	} else {
		log.Printf("Automatic backups are disabled")
	}
	go func() {
		<-shutdownContext.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("ThoughtGlean is listening on http://%s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func automaticBackups(ctx context.Context, notes *store.Store, files *attachments.Store, directory string, keep int, interval time.Duration) {
	for {
		report, err := backup.WriteAutomatic(ctx, notes, files, directory, keep)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("automatic backup failed: %v", err)
		} else {
			log.Printf("automatic backup created: %s (%d notes, %d attachments, sha256 %s)", report.Path, report.Notes, report.Attachments, report.SHA256)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
