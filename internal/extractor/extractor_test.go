package extractor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/takayuki/taiko_info/internal/db"
)

func TestParseISODate(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	tests := []struct {
		input       string
		expectError bool
		expectedYr  int
		expectedMo  time.Month
		expectedDay int
		expectedHr  int
		expectedMin int
	}{
		{"2026-08-30T19:30:00Z", false, 2026, time.August, 30, 19, 30},
		{"2026-08-30T19:30:00", false, 2026, time.August, 30, 19, 30},
		{"2026-08-30 19:30:00", false, 2026, time.August, 30, 19, 30},
		{"2026-08-30 19:30", false, 2026, time.August, 30, 19, 30},
		{"2026-08-30", false, 2026, time.August, 30, 0, 0},
		{"invalid-date-string", true, 0, 0, 0, 0, 0},
	}

	for _, tt := range tests {
		got, err := parseISODateInLocation(tt.input, loc)
		if tt.expectError && err == nil {
			t.Errorf("parseISODateInLocation(%q) expected error, got nil", tt.input)
		}
		if !tt.expectError {
			if err != nil {
				t.Errorf("parseISODateInLocation(%q) unexpected error: %v", tt.input, err)
			} else if got.Year() != tt.expectedYr || got.Month() != tt.expectedMo || got.Day() != tt.expectedDay || got.Hour() != tt.expectedHr || got.Minute() != tt.expectedMin {
				t.Errorf("parseISODateInLocation(%q) = %v; want %d-%02d-%02d %02d:%02d", tt.input, got, tt.expectedYr, tt.expectedMo, tt.expectedDay, tt.expectedHr, tt.expectedMin)
			}
		}
	}
}

func TestExtractedJSONParsing(t *testing.T) {
	sampleJSON := `[
		{
			"title": "Apresentação no Festival da Primavera",
			"description": "Chegar às 13:00 com uniforme completo",
			"event_date": "2026-09-12T14:30:00Z",
			"source_sender": "Sensei Carlos (5511988887777)"
		},
		{
			"title": "Ensaio Geral dos Tambores",
			"description": "Sede do grupo",
			"event_date": "2026-09-05T15:00:00Z",
			"source_sender": "Mariana"
		}
	]`

	var dtos []ExtractedEventDTO
	err := json.Unmarshal([]byte(sampleJSON), &dtos)
	if err != nil {
		t.Fatalf("Failed to unmarshal valid event json: %v", err)
	}

	if len(dtos) != 2 {
		t.Fatalf("Expected 2 events, got %d", len(dtos))
	}

	if dtos[0].Title != "Apresentação no Festival da Primavera" {
		t.Errorf("Unexpected title: %s", dtos[0].Title)
	}
}

func TestNormalizeCategory(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Apresentação", "apresentacao"},
		{"apresentacao", "apresentacao"},
		{"Show no parque", "apresentacao"},
		{"Festival de Inverno", "apresentacao"},
		{"Treino", "treino"},
		{"treino de taiko", "treino"},
		{"Ensaio Geral", "treino"},
		{"Oficina de postura", "treino"},
		{"Reunião financeira", "geral"},
		{"Avisos", "geral"},
		{"", "geral"},
	}

	for _, tt := range tests {
		got := normalizeCategory(tt.input)
		if got != tt.expected {
			t.Errorf("normalizeCategory(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExtractedJSONMarkdownFenceStripping(t *testing.T) {
	rawResponse := "```json\n[\n  {\"title\": \"Teste\", \"description\": \"Desc\", \"category\": \"treino\", \"event_date\": \"2026-08-30T10:00:00Z\", \"source_sender\": \"Admin\"}\n]\n```"

	cleaned := strings.TrimSpace(rawResponse)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	var dtos []ExtractedEventDTO
	if err := json.Unmarshal([]byte(cleaned), &dtos); err != nil {
		t.Fatalf("Failed to parse cleaned markdown-fenced json: %v", err)
	}

	if len(dtos) != 1 || dtos[0].Title != "Teste" || dtos[0].Category != "treino" {
		t.Fatalf("Unexpected parsed content: %+v", dtos)
	}
}

func TestExtractedJSONParsingWithUpdates(t *testing.T) {
	sampleJSON := `[
		{
			"id": 1,
			"action": "update",
			"title": "Ensaio Geral dos Tambores",
			"description": "Ensaio adiado para domingo às 16h",
			"category": "treino",
			"event_date": "2026-09-06T16:00:00",
			"source_sender": "Sensei Carlos"
		},
		{
			"id": null,
			"action": "create",
			"title": "Apresentação no Festival da Primavera",
			"description": "Novo festival confirmado",
			"category": "apresentacao",
			"event_date": "2026-09-12T14:30:00",
			"source_sender": "Diretoria"
		},
		{
			"id": "2",
			"action": "update",
			"title": "Reunião de Diretoria",
			"description": "Horário antecipado para 18h",
			"category": "geral",
			"event_date": "2026-09-10T18:00:00",
			"source_sender": "Ana Paula"
		}
	]`

	var dtos []ExtractedEventDTO
	err := json.Unmarshal([]byte(sampleJSON), &dtos)
	if err != nil {
		t.Fatalf("Failed to unmarshal update event json: %v", err)
	}

	if len(dtos) != 3 {
		t.Fatalf("Expected 3 events, got %d", len(dtos))
	}

	// First item: ID=1, action=update
	if dtos[0].ID == nil || *dtos[0].ID != 1 {
		t.Errorf("Expected ID 1, got %v", dtos[0].ID)
	}
	if dtos[0].Action != "update" {
		t.Errorf("Expected action 'update', got %s", dtos[0].Action)
	}

	// Second item: ID=nil, action=create
	if dtos[1].ID != nil {
		t.Errorf("Expected nil ID for new event, got %v", dtos[1].ID)
	}
	if dtos[1].Action != "create" {
		t.Errorf("Expected action 'create', got %s", dtos[1].Action)
	}

	// Third item: ID="2" (string parsed to int64)
	if dtos[2].ID == nil || *dtos[2].ID != 2 {
		t.Errorf("Expected ID 2 parsed from string, got %v", dtos[2].ID)
	}
	if dtos[2].Action != "update" {
		t.Errorf("Expected action 'update', got %s", dtos[2].Action)
	}
}

func TestFormatExistingEvents(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")

	// Test empty list
	emptyResult := formatExistingEvents(nil, loc)
	if !strings.Contains(emptyResult, "Nenhum evento") {
		t.Errorf("Expected empty message, got: %s", emptyResult)
	}

	// Test with events
	evDate := time.Date(2026, 8, 30, 15, 0, 0, 0, loc)
	events := []db.Event{
		{
			ID:           1,
			Title:        "Ensaio Geral",
			Category:     "treino",
			EventDate:    evDate,
			Description:  "Ensaio na sede",
			SourceSender: "Carlos Sensei",
		},
	}

	result := formatExistingEvents(events, loc)
	if !strings.Contains(result, "[ID: 1]") || !strings.Contains(result, "Ensaio Geral") || !strings.Contains(result, "2026-08-30 15:00:00") {
		t.Errorf("formatExistingEvents output unexpected: %s", result)
	}
}
