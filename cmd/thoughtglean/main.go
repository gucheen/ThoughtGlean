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

	server := &http.Server{
		Addr:              addr,
		Handler:           app.New(noteStore),
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
