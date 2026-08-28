package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/takayuki/taiko_info/internal/config"
	"github.com/takayuki/taiko_info/internal/db"
)

// ExtractedEventDTO defines the expected JSON output format from the LLM.
type ExtractedEventDTO struct {
	Title        string `json:"title"`
	Description  string `json:"description"`
	EventDate    string `json:"event_date"` // ISO 8601 string
	SourceSender string `json:"source_sender"`
}

// Extractor orchestrates message reading, LLM processing, and event persistence.
type Extractor struct {
	cfg        *config.Config
	database   *db.DB
	httpClient *http.Client
}

// New creates a new Extractor instance.
func New(cfg *config.Config, database *db.DB) *Extractor {
	return &Extractor{
		cfg:      cfg,
		database: database,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Run executes a single extraction batch on all unprocessed messages.
func (e *Extractor) Run(ctx context.Context) error {
	log.Println("[Extractor] Iniciando busca por mensagens não processadas...")

	messages, err := e.database.GetUnprocessedMessages(ctx, 0)
	if err != nil {
		return fmt.Errorf("falha ao consultar mensagens pendentes: %w", err)
	}

	if len(messages) == 0 {
		log.Println("[Extractor] Nenhuma mensagem nova com processed = 0 encontrada. Finalizado.")
		return nil
	}

	log.Printf("[Extractor] %d mensagem(ns) pendente(s) encontrada(s). Preparando chamada para LLM...", len(messages))

	// Collect message IDs for transaction marking
	var msgIDs []string
	var transcriptBuilder strings.Builder

	for _, m := range messages {
		msgIDs = append(msgIDs, m.ID)
		dateStr := m.CreatedAt.Format("2006-01-02 15:04:05 MST")
		transcriptBuilder.WriteString(fmt.Sprintf("[%s] %s: %s\n", dateStr, m.Sender, m.Message))
	}

	// Call Gemini API
	extractedDTOs, err := e.callGeminiAPI(ctx, transcriptBuilder.String())
	if err != nil {
		return fmt.Errorf("erro na extração com Gemini: %w", err)
	}

	log.Printf("[Extractor] LLM retornou %d evento(s) identificado(s).", len(extractedDTOs))

	// Convert DTOs to DB Event models
	var eventsToSave []db.Event
	for _, dto := range extractedDTOs {
		if strings.TrimSpace(dto.Title) == "" {
			continue
		}

		eventTime, err := parseISODate(dto.EventDate)
		if err != nil {
			log.Printf("[Extractor] Aviso: data inválida '%s' no evento '%s'. Usando data atual. Erro: %v",
				dto.EventDate, dto.Title, err)
			eventTime = time.Now()
		}

		eventsToSave = append(eventsToSave, db.Event{
			Title:        strings.TrimSpace(dto.Title),
			Description:  strings.TrimSpace(dto.Description),
			EventDate:    eventTime,
			SourceSender: strings.TrimSpace(dto.SourceSender),
		})
	}

	// Persist events and mark messages processed in a single transaction
	err = e.database.SaveEventsAndMarkProcessed(ctx, eventsToSave, msgIDs)
	if err != nil {
		return fmt.Errorf("falha ao persistir eventos e atualizar mensagens no banco: %w", err)
	}

	log.Printf("[Extractor] Sucesso: %d eventos inseridos e %d mensagens marcadas como processadas (processed = 1).",
		len(eventsToSave), len(msgIDs))

	return nil
}

// Gemini API Request/Response structs
type geminiRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerationConfig struct {
	ResponseMimeType string  `json:"responseMimeType"`
	Temperature      float64 `json:"temperature"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

func (e *Extractor) callGeminiAPI(ctx context.Context, messagesTranscript string) ([]ExtractedEventDTO, error) {
	if e.cfg.GeminiAPIKey == "" {
		return nil, errors.New("chave da API do Gemini não configurada (GEMINI_API_KEY vazia)")
	}

	now := time.Now()
	location, err := time.LoadLocation(e.cfg.ReferenceTZ)
	if err == nil {
		now = now.In(location)
	}

	systemPrompt := fmt.Sprintf(`Você é um assistente especialista em extrair eventos, compromissos, reuniões, ensaios, apresentações e prazos a partir de mensagens de um grupo do WhatsApp.

Contexto Temporal Atual:
- Data e Hora de Referência: %s (%s)
- Fuso Horário: %s

Diretrizes Obrigatórias:
1. Analise cuidadosamente todo o histórico de mensagens fornecido.
2. Identifique todos os eventos futuros ou compromissos combinados pelos participantes.
3. Resolva datas e horas relativas (ex: "hoje às 19h", "amanhã", "próximo sábado", "dia 15", "às 14:30") para o formato ISO 8601 estrito (YYYY-MM-DDTHH:MM:SSZ). Se o ano não for mencionado, assuma o ano corrente.
4. Extraia quem propôs ou confirmou a informação em 'source_sender'.
5. Se nenhuma mensagem contiver eventos ou compromissos agendados, retorne uma lista JSON vazia: []
6. Responda ESTRITAMENTE um array JSON válido sem markdown ou blocos de código adicionais.

Formato esperado de cada item:
{
  "title": "Título conciso do evento (ex: Ensaio Geral de Taiko, Apresentação no Festival)",
  "description": "Detalhes como local, o que levar, observações relevantes",
  "event_date": "2026-08-30T15:00:00Z",
  "source_sender": "Nome/Número do participante que anunciou"
}`, now.Format("2006-01-02 15:04:05"), now.Weekday().String(), e.cfg.ReferenceTZ)

	userPrompt := fmt.Sprintf("Histórico de mensagens recentes do WhatsApp:\n\n%s\n\nExtraia todos os eventos e retorne estritamente o array JSON:", messagesTranscript)

	reqPayload := geminiRequest{
		Contents: []geminiContent{
			{
				Role: "user",
				Parts: []geminiPart{
					{Text: userPrompt},
				},
			},
		},
		SystemInstruction: &geminiContent{
			Parts: []geminiPart{
				{Text: systemPrompt},
			},
		},
		GenerationConfig: geminiGenerationConfig{
			ResponseMimeType: "application/json",
			Temperature:      0.1,
		},
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("falha ao codificar payload para Gemini: %w", err)
	}

	model := strings.TrimPrefix(e.cfg.GeminiModel, "models/")
	modelsToTry := []string{model}
	for _, fallback := range []string{"gemini-2.0-flash", "gemini-2.5-flash", "gemini-1.5-flash-latest", "gemini-1.5-pro"} {
		if fallback != model {
			modelsToTry = append(modelsToTry, fallback)
		}
	}

	var respBytes []byte
	var lastStatus int
	var success bool

	for _, mName := range modelsToTry {
		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", mName, e.cfg.GeminiAPIKey)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("falha ao criar requisição HTTP: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := e.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("falha na chamada HTTP para API do Gemini: %w", err)
		}

		respBytes, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("falha ao ler resposta da API do Gemini: %w", err)
		}

		lastStatus = resp.StatusCode
		if resp.StatusCode == http.StatusOK {
			success = true
			break
		}

		if resp.StatusCode == http.StatusNotFound {
			log.Printf("[Extractor] Modelo '%s' indisponível (404), tentando fallback...", mName)
			continue
		}

		return nil, fmt.Errorf("Gemini API retornou status %d: %s", resp.StatusCode, string(respBytes))
	}

	if !success {
		return nil, fmt.Errorf("Gemini API retornou status %d: %s", lastStatus, string(respBytes))
	}

	var geminiResp geminiResponse
	if err := json.Unmarshal(respBytes, &geminiResp); err != nil {
		return nil, fmt.Errorf("falha ao decodificar resposta do Gemini: %w", err)
	}

	if geminiResp.Error != nil {
		return nil, fmt.Errorf("erro retornado pelo Gemini: %s (código %d)", geminiResp.Error.Message, geminiResp.Error.Code)
	}

	if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return nil, errors.New("resposta do Gemini não conteve partes de conteúdo válidas")
	}

	rawJSONText := strings.TrimSpace(geminiResp.Candidates[0].Content.Parts[0].Text)

	// Clean any markdown fences if model returned them despite json mime type
	rawJSONText = strings.TrimPrefix(rawJSONText, "```json")
	rawJSONText = strings.TrimPrefix(rawJSONText, "```")
	rawJSONText = strings.TrimSuffix(rawJSONText, "```")
	rawJSONText = strings.TrimSpace(rawJSONText)

	if rawJSONText == "" || rawJSONText == "null" {
		return []ExtractedEventDTO{}, nil
	}

	var dtos []ExtractedEventDTO
	if err := json.Unmarshal([]byte(rawJSONText), &dtos); err != nil {
		// Attempt parsing as single object if model returned non-array
		var single ExtractedEventDTO
		if errSingle := json.Unmarshal([]byte(rawJSONText), &single); errSingle == nil && single.Title != "" {
			return []ExtractedEventDTO{single}, nil
		}
		return nil, fmt.Errorf("falha ao fazer unmarshal do JSON extraído (%s): %w", rawJSONText, err)
	}

	return dtos, nil
}

func parseISODate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.000Z",
		"2006-01-02",
	}

	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("formato de data desconhecido: %s", s)
}
