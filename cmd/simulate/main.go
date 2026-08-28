package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/takayuki/taiko_info/internal/db"
)

func main() {
	log.Println("=== [SIMULAÇÃO] Iniciando pipeline de teste local ===")

	dbPath := "app.db"
	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("Erro ao abrir banco: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	// 1. Simular mensagens chegando do WhatsApp
	log.Println("\n--- 1. Inserindo mensagens simuladas recebidas do WhatsApp no SQLite ---")
	now := time.Now()

	sampleMessages := []struct {
		id     string
		sender string
		text   string
		delay  time.Duration
	}{
		{
			id:     "WA-SIM-001",
			sender: "Carlos Sensei (5511988881111)",
			text:   "Bom dia pessoal! Atenção: nosso Ensaio Geral para o Festival da Primavera será neste sábado, 30 de Agosto às 15:00 na sede. Tragam bachi e uniforme completo.",
			delay:  -3 * time.Hour,
		},
		{
			id:     "WA-SIM-002",
			sender: "Mariana (5511977772222)",
			text:   "Confirmado Sensei! Eu levo a água e a lista de presença.",
			delay:  -2 * time.Hour,
		},
		{
			id:     "WA-SIM-003",
			sender: "Renato - Diretoria (5511966663333)",
			text:   "Lembrando também que nossa Apresentação no Festival de Verão está confirmada para 12 de Setembro às 14:30 no Parque da Cidade.",
			delay:  -1 * time.Hour,
		},
		{
			id:     "WA-SIM-004",
			sender: "Ana Paula (5511955554444)",
			text:   "A reunião de alinhamento financeiro foi marcada para a próxima terça-feira às 19:30 via Google Meet.",
			delay:  -30 * time.Minute,
		},
	}

	for _, m := range sampleMessages {
		msgTime := now.Add(m.delay)
		err := database.SaveMessage(ctx, m.id, m.sender, m.text, msgTime)
		if err != nil {
			log.Fatalf("Erro ao salvar mensagem: %v", err)
		}
		fmt.Printf("✔ [Mensagem Capturada] [%s] %s: %s\n", m.id, m.sender, m.text)
	}

	// 2. Verificar mensagens com processed = 0
	unprocessed, err := database.GetUnprocessedMessages(ctx, 0)
	if err != nil {
		log.Fatalf("Erro ao buscar mensagens pendentes: %v", err)
	}
	fmt.Printf("\n--- 2. Mensagens pendentes de processamento na tabela 'messages' (processed = 0): %d ---\n", len(unprocessed))

	// 3. Simular extração estruturada de eventos (como a LLM Gemini extrai)
	fmt.Println("\n--- 3. Executando extração inteligente e transação no banco ---")
	simulatedEvents := []db.Event{
		{
			Title:        "Ensaio Geral - Festival da Primavera",
			Description:  "Ensaio na sede com todos os instrumentos. Levar bachi e uniforme completo.",
			EventDate:    now.Add(48 * time.Hour), // Futuro próximo
			SourceSender: "Carlos Sensei (5511988881111)",
		},
		{
			Title:        "Apresentação no Festival de Verão",
			Description:  "Apresentação principal do grupo no Parque da Cidade.",
			EventDate:    now.Add(15 * 24 * time.Hour), // Futuro
			SourceSender: "Renato - Diretoria (5511966663333)",
		},
		{
			Title:        "Reunião de Alinhamento Financeiro",
			Description:  "Reunião de planejamento por videoconferência via Google Meet.",
			EventDate:    now.Add(5 * 24 * time.Hour),
			SourceSender: "Ana Paula (5511955554444)",
		},
		{
			Title:        "Oficina Básica de Taiko para Iniciantes",
			Description:  "Treinamento de postura e ritmo com os novos integrantes.",
			EventDate:    now.Add(-7 * 24 * time.Hour), // Evento passado
			SourceSender: "Carlos Sensei (5511988881111)",
		},
	}

	var msgIDs []string
	for _, m := range unprocessed {
		msgIDs = append(msgIDs, m.ID)
	}

	err = database.SaveEventsAndMarkProcessed(ctx, simulatedEvents, msgIDs)
	if err != nil {
		log.Fatalf("Erro na transação de eventos: %v", err)
	}
	fmt.Printf("✔ Transação concluída com sucesso! %d eventos gravados e %d mensagens marcadas como processed = 1.\n",
		len(simulatedEvents), len(msgIDs))

	// 4. Exibir métricas finais
	totalEvents, pendingMsgs, processedMsgs, err := database.GetStats(ctx)
	if err != nil {
		log.Fatalf("Erro ao consultar métricas: %v", err)
	}

	fmt.Println("\n--- 4. Resumo no Banco SQLite (app.db) ---")
	fmt.Printf("• Total de Eventos Registrados: %d\n", totalEvents)
	fmt.Printf("• Mensagens Pendentes: %d\n", pendingMsgs)
	fmt.Printf("• Mensagens Processadas: %d\n", processedMsgs)
	fmt.Println("\n=== [SIMULAÇÃO CONCLUÍDA COM SUCESSO] ===")
}
