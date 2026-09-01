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
	ID           *int64 `json:"id,omitempty"`
	Action       string `json:"action,omitempty"` // "create", "update"
	Title        string `json:"title"`
	Description  string `json:"description"`
	Category     string `json:"category"` // "apresentacao", "treino", "geral"
	EventDate    string `json:"event_date"` // ISO 8601 string
	SourceSender string `json:"source_sender"`
}

// UnmarshalJSON customizes unmarshaling to gracefully handle diverse id representations (integer, float, string, or null).
func (d *ExtractedEventDTO) UnmarshalJSON(data []byte) error {
	type Alias ExtractedEventDTO
	aux := &struct {
		RawID any `json:"id"`
		*Alias
	}{
		Alias: (*Alias)(d),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.RawID != nil {
		switch v := aux.RawID.(type) {
		case float64:
			if v > 0 {
				id := int64(v)
				d.ID = &id
			}
		case int64:
			if v > 0 {
				d.ID = &v
			}
		case int:
			if v > 0 {
				id := int64(v)
				d.ID = &id
			}
		case string:
			v = strings.TrimSpace(v)
			if v != "" && v != "null" && v != "0" {
				var parsed int64
				if _, err := fmt.Sscanf(v, "%d", &parsed); err == nil && parsed > 0 {
					d.ID = &parsed
				}
			}
		}
	}
	return nil
}

// formatExistingEvents formats current events into a clean reference list for LLM context.
func formatExistingEvents(events []db.Event, loc *time.Location) string {
	if len(events) == 0 {
		return "(Nenhum evento previamente cadastrado no banco de dados)"
	}
	var sb strings.Builder
	for _, ev := range events {
		dateStr := ev.EventDate.In(loc).Format("2006-01-02 15:04:05")
		sb.WriteString(fmt.Sprintf("- [ID: %d] Título: %q | Categoria: %s | Data/Hora: %s | Descrição: %q | Informado por: %q\n",
			ev.ID, ev.Title, ev.Category, dateStr, ev.Description, ev.SourceSender))
	}
	return sb.String()
}

// normalizeCategory standardizes the category string to "apresentacao", "treino", or "geral".
func normalizeCategory(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(s, "apresent") || strings.Contains(s, "show") || strings.Contains(s, "festival") || strings.Contains(s, "demonstra"):
		return "apresentacao"
	case strings.Contains(s, "trein") || strings.Contains(s, "ensaio") || strings.Contains(s, "oficina") || strings.Contains(s, "pratic"):
		return "treino"
	default:
		return "geral"
	}
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

	loc, _ := time.LoadLocation(e.cfg.ReferenceTZ)
	if loc == nil {
		loc = time.Local
	}

	// Fetch existing events so LLM can identify and update changed dates instead of creating duplicates
	existingEvents, err := e.database.GetAllEventsInLocation(ctx, loc)
	if err != nil {
		log.Printf("[Extractor] Aviso: falha ao consultar eventos existentes para contexto: %v", err)
		existingEvents = nil
	}

	// Collect message IDs for transaction marking
	var msgIDs []string
	var transcriptBuilder strings.Builder

	for _, m := range messages {
		msgIDs = append(msgIDs, m.ID)
		dateStr := m.CreatedAt.Format("2006-01-02 15:04:05 MST")
		transcriptBuilder.WriteString(fmt.Sprintf("[%s] %s: %s\n", dateStr, m.Sender, m.Message))
	}

	// Call Gemini API with existing events context
	extractedDTOs, err := e.callGeminiAPI(ctx, transcriptBuilder.String(), existingEvents)
	if err != nil {
		return fmt.Errorf("erro na extração com Gemini: %w", err)
	}

	log.Printf("[Extractor] LLM retornou %d evento(s) identificado(s)/atualizado(s).", len(extractedDTOs))

	// Map existing events by ID for validation
	existingByID := make(map[int64]db.Event)
	for _, ev := range existingEvents {
		existingByID[ev.ID] = ev
	}

	// Convert DTOs to DB Event models
	var eventsToSave []db.Event
	var updatedCount, createdCount int

	for _, dto := range extractedDTOs {
		if strings.TrimSpace(dto.Title) == "" {
			continue
		}

		eventTime, err := parseISODateInLocation(dto.EventDate, loc)
		if err != nil {
			log.Printf("[Extractor] Aviso: data inválida '%s' no evento '%s'. Usando data atual. Erro: %v",
				dto.EventDate, dto.Title, err)
			eventTime = time.Now().In(loc)
		}

		var targetID int64
		if dto.ID != nil && *dto.ID > 0 {
			if _, exists := existingByID[*dto.ID]; exists {
				targetID = *dto.ID
			}
		}

		// Fallback: if action indicates update or if DTO matches an existing event's title
		if targetID == 0 && (dto.Action == "update" || dto.Action == "alter" || dto.Action == "change") {
			for _, ex := range existingEvents {
				if strings.EqualFold(strings.TrimSpace(ex.Title), strings.TrimSpace(dto.Title)) {
					targetID = ex.ID
					break
				}
			}
		}

		if targetID > 0 {
			updatedCount++
			log.Printf("[Extractor] Alteração de data/evento detectada: Atualizando Evento ID %d (%s) para nova data %s",
				targetID, dto.Title, eventTime.Format("2006-01-02 15:04:05"))
		} else {
			createdCount++
			log.Printf("[Extractor] Novo evento detectado: Criando '%s' para a data %s",
				dto.Title, eventTime.Format("2006-01-02 15:04:05"))
		}

		eventsToSave = append(eventsToSave, db.Event{
			ID:           targetID,
			Title:        strings.TrimSpace(dto.Title),
			Description:  strings.TrimSpace(dto.Description),
			Category:     normalizeCategory(dto.Category),
			EventDate:    eventTime,
			SourceSender: strings.TrimSpace(dto.SourceSender),
		})
	}

	// Persist events (insert or update) and mark messages processed in a single transaction
	err = e.database.SaveEventsAndMarkProcessed(ctx, eventsToSave, msgIDs)
	if err != nil {
		return fmt.Errorf("falha ao persistir eventos e atualizar mensagens no banco: %w", err)
	}

	log.Printf("[Extractor] Sucesso: %d evento(s) criado(s), %d evento(s) atualizado(s)/alterado(s) e %d mensagens marcadas como processadas (processed = 1).",
		createdCount, updatedCount, len(msgIDs))

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

func (e *Extractor) callGeminiAPI(ctx context.Context, messagesTranscript string, existingEvents []db.Event) ([]ExtractedEventDTO, error) {
	if e.cfg.GeminiAPIKey == "" {
		return nil, errors.New("chave da API do Gemini não configurada (GEMINI_API_KEY vazia)")
	}

	now := time.Now()
	location, err := time.LoadLocation(e.cfg.ReferenceTZ)
	if err == nil {
		now = now.In(location)
	}

	systemPrompt := fmt.Sprintf(`Você é um assistente especialista em extrair e sincronizar eventos, compromissos, reuniões, ensaios, apresentações e prazos a partir de mensagens de um grupo do WhatsApp.

Contexto Temporal Atual:
- Data e Hora Atual de Referência: %s (%s)
- Fuso Horário Local: %s (Horário de Brasília)

Eventos Atualmente Cadastrados no Banco de Dados:
%s

Diretrizes Obrigatórias:
1. Analise cuidadosamente todo o histórico de mensagens fornecido.
2. Identifique todos os eventos futuros ou compromissos combinados pelos participantes, bem como ALTERAÇÕES de eventos já existentes.
3. DETECÇÃO E ALTERAÇÃO DE DATAS / REAGENDAMENTOS:
   - Verifique atentamente se alguma mensagem traz uma ALTERAÇÃO, ADIAMENTO, MUDANÇA DE DATA/HORÁRIO, CANCELAMENTO/REAGENDAMENTO ou atualização de local/detalhes de um evento já cadastrado na lista 'Eventos Atualmente Cadastrados no Banco de Dados'.
   - Se uma mensagem alterar a data/horário ou detalhes de um evento existente:
     * NÃO crie um novo evento duplicado.
     * Preencha o campo 'id' com o número do ID do evento existente correspondente.
     * Preencha o campo 'action' como "update".
     * Preencha 'event_date' com a NOVA data/horário estipulado na mensagem.
     * Atualize 'title', 'description' e 'source_sender' com as informações mais recentes da mensagem.
   - Se for um NOVO compromisso/evento que ainda não existe no cadastro:
     * Preencha 'id': null.
     * Preencha 'action': "create".
     * Preencha 'event_date' com a data e horário agendados.
   - Se houver múltiplas mensagens no histórico propondo e depois alterando a mesma data (ex: propõe dia 10 e depois altera para dia 12), consolide apenas a data final corrigida.
4. Se a mensagem mencionar um horário (ex: "19:30", "15:00", "às 14h"), preserve ESTRITAMENTE esse horário local no campo 'event_date' formatado como 'YYYY-MM-DDTHH:MM:SS' (NÃO subtraia horas e NÃO adicione Z). Se o ano não for mencionado, assuma o ano corrente.
5. Extraia quem propôs, confirmou ou alterou a informação em 'source_sender'.
6. Categorize cada evento no campo 'category' como:
   - "apresentacao" para apresentações públicas, shows, festivais, demonstrações e eventos culturais;
   - "treino" para ensaios, ensaio geral, treinos técnicos e oficinas práticas de taiko;
   - "geral" para reuniões de alinhamento, decisões financeiras, confraternizações e avisos gerais.
7. Se nenhuma mensagem contiver novos eventos ou alterações em eventos existentes, retorne uma lista JSON vazia: []
8. Responda ESTRITAMENTE um array JSON válido sem markdown ou blocos de código adicionais.

Formato esperado de cada item:
{
  "id": 1,
  "action": "update",
  "title": "Título do evento (ex: Ensaio Geral de Taiko)",
  "description": "Detalhes como local, horário completo, o que levar, observações relevantes",
  "category": "apresentacao | treino | geral",
  "event_date": "2026-08-30T19:30:00",
  "source_sender": "Nome/Número do participante que anunciou ou alterou"
}`, now.Format("2006-01-02 15:04:05"), now.Weekday().String(), e.cfg.ReferenceTZ, formatExistingEvents(existingEvents, location))

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
	for _, fallback := range []string{
		"gemini-2.5-flash",
		"gemini-flash-latest",
		"gemini-2.5-flash-lite",
		"gemini-flash-lite-latest",
		"gemini-2.0-flash",
		"gemini-1.5-flash",
	} {
		if fallback != model {
			modelsToTry = append(modelsToTry, fallback)
		}
	}

	apiVersions := []string{"v1beta", "v1"}
	var respBytes []byte
	var lastStatus int
	var success bool
	var usedModel, usedVersion string

	for _, apiVer := range apiVersions {
		for _, mName := range modelsToTry {
			url := fmt.Sprintf("https://generativelanguage.googleapis.com/%s/models/%s:generateContent?key=%s", apiVer, mName, e.cfg.GeminiAPIKey)
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
				usedModel = mName
				usedVersion = apiVer
				break
			}

			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
				log.Printf("[Extractor] Modelo '%s' retornou status %d (%s), tentando próximo modelo...", mName, resp.StatusCode, resp.Status)
				time.Sleep(1 * time.Second)
				continue
			}

			return nil, fmt.Errorf("Gemini API retornou status %d: %s", resp.StatusCode, string(respBytes))
		}
		if success {
			break
		}
	}

	if success {
		log.Printf("[Extractor] Conexão bem-sucedida com modelo '%s' (%s)", usedModel, usedVersion)
	} else {
		// Diagnose available models from API key
		diagURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models?key=%s", e.cfg.GeminiAPIKey)
		if diagResp, err := e.httpClient.Get(diagURL); err == nil {
			defer diagResp.Body.Close()
			diagBytes, _ := io.ReadAll(diagResp.Body)
			log.Printf("[Extractor] Diagnóstico de modelos disponíveis para esta chave: %s", string(diagBytes))
		}
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

func parseISODateInLocation(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	// Strip Z/z suffix to prevent treating local times as UTC
	sClean := strings.TrimSuffix(s, "Z")
	sClean = strings.TrimSuffix(sClean, "z")

	formats := []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	}

	for _, f := range formats {
		if t, err := time.ParseInLocation(f, sClean, loc); err == nil {
			return t, nil
		}
	}

	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(loc), nil
	}

	return time.Time{}, fmt.Errorf("formato de data desconhecido: %s", s)
}
