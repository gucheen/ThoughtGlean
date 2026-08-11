package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"thoughtglean/internal/app"
	"thoughtglean/internal/attachments"
	"thoughtglean/internal/store"
)

func main() {
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
	application := app.New(noteStore)
	application.SetAttachmentStore(attachmentStore)
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

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
