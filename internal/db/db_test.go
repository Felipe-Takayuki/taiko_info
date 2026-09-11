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
			Category:     "geral",
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
	if allEvents[0].Title != "Reunião de Diretoria" || allEvents[0].Category != "geral" {
		t.Errorf("Unexpected event: %+v", allEvents[0])
	}

	totalEvents, unprocessedCount, processedCount, err := database.GetStats(ctx)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}
	if totalEvents != 1 || unprocessedCount != 0 || processedCount != 2 {
		t.Errorf("Unexpected stats: total=%d, unprocessed=%d, processed=%d", totalEvents, unprocessedCount, processedCount)
	}
}

func TestDB_UpdateExistingEventDate(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// Initial message and event creation
	_ = database.SaveMessage(ctx, "msg-201", "Carlos Sensei", "Ensaio sábado dia 30/08 às 15:00 na sede", time.Now())
	initialDate := time.Date(2026, 8, 30, 15, 0, 0, 0, time.Local)
	initialEvents := []Event{
		{
			Title:        "Ensaio Geral",
			Description:  "Ensaio na sede",
			Category:     "treino",
			EventDate:    initialDate,
			SourceSender: "Carlos Sensei",
		},
	}
	err := database.SaveEventsAndMarkProcessed(ctx, initialEvents, []string{"msg-201"})
	if err != nil {
		t.Fatalf("Initial SaveEventsAndMarkProcessed failed: %v", err)
	}

	allEvents, err := database.GetAllEvents(ctx)
	if err != nil || len(allEvents) != 1 {
		t.Fatalf("Expected 1 event, got %d (err: %v)", len(allEvents), err)
	}
	createdID := allEvents[0].ID
	if createdID == 0 {
		t.Fatalf("Expected non-zero createdID")
	}

	// New message altering the date of the existing event
	_ = database.SaveMessage(ctx, "msg-202", "Carlos Sensei", "Pessoal, o ensaio geral mudou para domingo 31/08 às 16:00!", time.Now())
	newDate := time.Date(2026, 8, 31, 16, 0, 0, 0, time.Local)
	updatedEvents := []Event{
		{
			ID:           createdID, // Existing event ID being altered
			Title:        "Ensaio Geral",
			Description:  "Ensaio adiado para domingo às 16:00 na sede",
			Category:     "treino",
			EventDate:    newDate,
			SourceSender: "Carlos Sensei",
		},
	}

	err = database.SaveEventsAndMarkProcessed(ctx, updatedEvents, []string{"msg-202"})
	if err != nil {
		t.Fatalf("Updated SaveEventsAndMarkProcessed failed: %v", err)
	}

	// Verify that the total count of events is STILL 1 (no duplicate created)
	allEventsAfterUpdate, err := database.GetAllEvents(ctx)
	if err != nil {
		t.Fatalf("GetAllEvents failed: %v", err)
	}
	if len(allEventsAfterUpdate) != 1 {
		t.Fatalf("Expected still 1 event after date alteration, but found %d", len(allEventsAfterUpdate))
	}

	// Verify that the event date and details were updated
	updatedEv := allEventsAfterUpdate[0]
	if updatedEv.ID != createdID {
		t.Errorf("Expected ID %d, got %d", createdID, updatedEv.ID)
	}
	if !updatedEv.EventDate.Equal(newDate) {
		t.Errorf("Expected updated event_date %v, got %v", newDate, updatedEv.EventDate)
	}
	if updatedEv.Description != "Ensaio adiado para domingo às 16:00 na sede" {
		t.Errorf("Expected updated description, got %q", updatedEv.Description)
	}

	// Verify GetEventByID
	eventByID, err := database.GetEventByID(ctx, createdID)
	if err != nil {
		t.Fatalf("GetEventByID failed: %v", err)
	}
	if eventByID.ID != createdID || !eventByID.EventDate.Equal(newDate) {
		t.Errorf("GetEventByID returned unexpected data: %+v", eventByID)
	}
}

func TestDB_CancelExistingEvent(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	_ = database.SaveMessage(ctx, "msg-301", "Carlos Sensei", "Ensaio sábado dia 30/08 às 15:00 na sede", time.Now())
	initialDate := time.Date(2026, 8, 30, 15, 0, 0, 0, time.Local)
	initialEvents := []Event{
		{
			Title:        "Ensaio Geral",
			Description:  "Ensaio na sede",
			Category:     "treino",
			Status:       "agendado",
			EventDate:    initialDate,
			SourceSender: "Carlos Sensei",
		},
	}
	err := database.SaveEventsAndMarkProcessed(ctx, initialEvents, []string{"msg-301"})
	if err != nil {
		t.Fatalf("Initial SaveEventsAndMarkProcessed failed: %v", err)
	}

	allEvents, err := database.GetAllEvents(ctx)
	if err != nil || len(allEvents) != 1 {
		t.Fatalf("Expected 1 event, got %d (err: %v)", len(allEvents), err)
	}
	createdID := allEvents[0].ID
	if allEvents[0].Status != "agendado" {
		t.Errorf("Expected status 'agendado', got %q", allEvents[0].Status)
	}

	// Cancel message
	_ = database.SaveMessage(ctx, "msg-302", "Carlos Sensei", "Pessoal, o ensaio geral deste sábado está cancelado devido à chuva.", time.Now())
	canceledEvents := []Event{
		{
			ID:           createdID,
			Title:        "Ensaio Geral",
			Description:  "Cancelado devido à chuva.",
			Category:     "treino",
			Status:       "cancelado",
			EventDate:    initialDate,
			SourceSender: "Carlos Sensei",
		},
	}

	err = database.SaveEventsAndMarkProcessed(ctx, canceledEvents, []string{"msg-302"})
	if err != nil {
		t.Fatalf("Cancel SaveEventsAndMarkProcessed failed: %v", err)
	}

	evAfterCancel, err := database.GetEventByID(ctx, createdID)
	if err != nil {
		t.Fatalf("GetEventByID failed: %v", err)
	}
	if evAfterCancel.Status != "cancelado" {
		t.Errorf("Expected status 'cancelado', got %q", evAfterCancel.Status)
	}
}
