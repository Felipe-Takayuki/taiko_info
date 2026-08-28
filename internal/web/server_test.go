package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takayuki/taiko_info/internal/config"
	"github.com/takayuki/taiko_info/internal/db"
)

func TestWeb_IndexAndAPIEndpoints(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_web.db")

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test db: %v", err)
	}
	defer database.Close()

	ctx := context.Background()
	_ = database.SaveMessage(ctx, "m1", "Admin", "Aviso geral", time.Now())

	eventTime := time.Now().Add(48 * time.Hour)
	_ = database.SaveEventsAndMarkProcessed(ctx, []db.Event{
		{
			Title:        "Festival Cultural do Taiko",
			Description:  "Grande apresentação no palco principal",
			EventDate:    eventTime,
			SourceSender: "Diretoria (5511999990000)",
		},
	}, []string{"m1"})

	cfg := &config.Config{
		HTTPPort:    8080,
		ReferenceTZ: "America/Sao_Paulo",
	}

	server, err := New(cfg, database)
	if err != nil {
		t.Fatalf("Failed to instantiate web server: %v", err)
	}

	// Test GET /
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	server.handleIndex(w, req)
	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / returned status %d; want 200", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Festival Cultural do Taiko") {
		t.Errorf("Response body missing expected event title. Got:\n%s", body)
	}
	if !strings.Contains(body, "Mural de Eventos") {
		t.Errorf("Response body missing header title.")
	}

	// Test GET /api/events
	reqAPI := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	wAPI := httptest.NewRecorder()
	server.handleAPIEvents(wAPI, reqAPI)
	if wAPI.Result().StatusCode != http.StatusOK {
		t.Errorf("GET /api/events status %d; want 200", wAPI.Result().StatusCode)
	}
	if !strings.Contains(wAPI.Body.String(), "Festival Cultural do Taiko") {
		t.Errorf("JSON API missing event.")
	}

	// Test GET /healthz
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	wHealth := httptest.NewRecorder()
	server.handleHealthz(wHealth, reqHealth)
	if wHealth.Result().StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status %d; want 200", wHealth.Result().StatusCode)
	}
}
