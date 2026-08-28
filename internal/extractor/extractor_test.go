package extractor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func TestExtractedJSONMarkdownFenceStripping(t *testing.T) {
	rawResponse := "```json\n[\n  {\"title\": \"Teste\", \"description\": \"Desc\", \"event_date\": \"2026-08-30T10:00:00Z\", \"source_sender\": \"Admin\"}\n]\n```"

	cleaned := strings.TrimSpace(rawResponse)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	var dtos []ExtractedEventDTO
	if err := json.Unmarshal([]byte(cleaned), &dtos); err != nil {
		t.Fatalf("Failed to parse cleaned markdown-fenced json: %v", err)
	}

	if len(dtos) != 1 || dtos[0].Title != "Teste" {
		t.Fatalf("Unexpected parsed content: %+v", dtos)
	}
}
