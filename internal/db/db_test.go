package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setupTestDB(t *testing.T) (*DB, func()) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}

	cleanup := func() {
		_ = database.Close()
		_ = os.Remove(dbPath)
	}

	return database, cleanup
}

func TestDB_SaveMessageAndGetUnprocessed(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	msgTime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	err := database.SaveMessage(ctx, "msg-001", "Maria", "Ensaio sábado às 15h!", msgTime)
	if err != nil {
		t.Fatalf("SaveMessage failed: %v", err)
	}

	// Save duplicate ID with updated content (idempotency)
	err = database.SaveMessage(ctx, "msg-001", "Maria", "Ensaio sábado às 15h na sede!", msgTime)
	if err != nil {
		t.Fatalf("SaveMessage idempotency update failed: %v", err)
	}

	// Save second message
	err = database.SaveMessage(ctx, "msg-002", "João", "Confirmado!", msgTime.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("SaveMessage 2 failed: %v", err)
	}

	unprocessed, err := database.GetUnprocessedMessages(ctx, 10)
	if err != nil {
		t.Fatalf("GetUnprocessedMessages failed: %v", err)
	}

	if len(unprocessed) != 2 {
		t.Fatalf("Expected 2 unprocessed messages, got %d", len(unprocessed))
	}

	if unprocessed[0].ID != "msg-001" || unprocessed[0].Message != "Ensaio sábado às 15h na sede!" {
		t.Errorf("Unexpected message content: %+v", unprocessed[0])
	}
}

func TestDB_SaveEventsAndMarkProcessed(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	_ = database.SaveMessage(ctx, "msg-101", "Ana", "Reunião de diretoria dia 30/08 às 19:00", time.Now())
	_ = database.SaveMessage(ctx, "msg-102", "Pedro", "Ok", time.Now())

	eventDate := time.Date(2026, 8, 30, 19, 0, 0, 0, time.UTC)
	events := []Event{
		{
			Title:        "Reunião de Diretoria",
			Description:  "Alinhamento geral do grupo",
			EventDate:    eventDate,
			SourceSender: "Ana",
		},
	}

	msgIDs := []string{"msg-101", "msg-102"}

	err := database.SaveEventsAndMarkProcessed(ctx, events, msgIDs)
	if err != nil {
		t.Fatalf("SaveEventsAndMarkProcessed failed: %v", err)
	}

	// Check unprocessed messages are now 0
	unprocessed, err := database.GetUnprocessedMessages(ctx, 0)
	if err != nil {
		t.Fatalf("GetUnprocessedMessages failed: %v", err)
	}
	if len(unprocessed) != 0 {
		t.Errorf("Expected 0 unprocessed messages, got %d", len(unprocessed))
	}

	// Check events inserted
	allEvents, err := database.GetAllEvents(ctx)
	if err != nil {
		t.Fatalf("GetAllEvents failed: %v", err)
	}
	if len(allEvents) != 1 {
		t.Fatalf("Expected 1 event, got %d", len(allEvents))
	}
	if allEvents[0].Title != "Reunião de Diretoria" {
		t.Errorf("Unexpected event title: %s", allEvents[0].Title)
	}

	totalEvents, unprocessedCount, processedCount, err := database.GetStats(ctx)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}
	if totalEvents != 1 || unprocessedCount != 0 || processedCount != 2 {
		t.Errorf("Unexpected stats: total=%d, unprocessed=%d, processed=%d", totalEvents, unprocessedCount, processedCount)
	}
}
