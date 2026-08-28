package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/takayuki/taiko_info/internal/config"
	"github.com/takayuki/taiko_info/internal/db"
	"github.com/takayuki/taiko_info/internal/extractor"
	"github.com/takayuki/taiko_info/internal/listener"
	"github.com/takayuki/taiko_info/internal/web"
)

const version = "1.0.0"

func printUsage() {
	fmt.Println(`Taiko Info - Gerenciador Minimalista e Autônomo de Eventos WhatsApp

Uso:
  taiko <comando> [flags]

Comandos Disponíveis:
  listener, run-listener    Inicia o daemon do WhatsApp (Whatsmeow + autenticação via QR Code)
  groups, list-groups       Lista todos os grupos do WhatsApp da conta com seus respectivos JIDs
  extractor, run-extractor  Executa uma extração de eventos via LLM (Gemini) das mensagens pendentes
  reprocess                 Limpa eventos anteriores e reprocessa todas as mensagens do banco
  web, run-web              Inicia o servidor HTTP do Mural Web (porta padrão: 8080)
  all                       Inicia tanto o Listener quanto o Servidor Web concorrentemente
  version                   Exibe a versão do sistema
  help                      Exibe esta mensagem de ajuda

Variáveis de Ambiente (.env ou export):
  DB_PATH             Caminho do banco SQLite (padrão: app.db)
  WHATSAPP_GROUP_JID  JID do grupo do WhatsApp a ser monitorado
  GEMINI_API_KEY      Chave de API do Google Gemini
  GEMINI_MODEL        Modelo Gemini (padrão: gemini-1.5-flash)
  HTTP_PORT           Porta do servidor HTTP (padrão: 8080)
  TIMEZONE            Fuso horário de referência (padrão: America/Sao_Paulo)

Exemplos:
  go run main.go listener
  go run main.go extractor
  go run main.go web -port=8080
  go run main.go all`)
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cfg := config.Load()
	command := os.Args[1]

	// Handle global help or version
	if command == "help" || command == "-h" || command == "--help" {
		printUsage()
		return
	}
	if command == "version" || command == "-v" || command == "--version" {
		fmt.Printf("taiko version %s\n", version)
		return
	}

	// Initialize SQLite Database
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("[Main] Erro ao conectar ao banco de dados SQLite (%s): %v", cfg.DBPath, err)
	}
	defer database.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("[Main] Interrupção detectada, encerrando...")
		cancel()
	}()

	switch command {
	case "listener", "run-listener":
		runListener(ctx, cfg, database, os.Args[2:])

	case "groups", "list-groups":
		runListGroups(ctx, cfg, database)

	case "extractor", "run-extractor":
		runExtractor(ctx, cfg, database, os.Args[2:])

	case "reprocess", "run-reprocess":
		runReprocess(ctx, cfg, database, os.Args[2:])

	case "web", "run-web":
		runWeb(ctx, cfg, database, os.Args[2:])

	case "all":
		runAll(ctx, cfg, database, os.Args[2:])

	default:
		fmt.Fprintf(os.Stderr, "Erro: comando desconhecido '%s'\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func runListener(ctx context.Context, cfg *config.Config, database *db.DB, args []string) {
	fs := flag.NewFlagSet("listener", flag.ExitOnError)
	groupJID := fs.String("group", cfg.WhatsAppGroup, "JID do grupo do WhatsApp")
	_ = fs.Parse(args)

	if *groupJID != "" {
		cfg.WhatsAppGroup = *groupJID
	}

	srv := listener.New(cfg, database)
	if err := srv.Run(ctx); err != nil {
		log.Fatalf("[Listener] Falha fatal: %v", err)
	}
}

func runListGroups(ctx context.Context, cfg *config.Config, database *db.DB) {
	srv := listener.New(cfg, database)
	if err := srv.ListGroups(ctx); err != nil {
		log.Fatalf("[Groups] Erro ao listar grupos: %v", err)
	}
}

func runExtractor(ctx context.Context, cfg *config.Config, database *db.DB, args []string) {
	fs := flag.NewFlagSet("extractor", flag.ExitOnError)
	apiKey := fs.String("api-key", cfg.GeminiAPIKey, "Chave de API do Gemini")
	model := fs.String("model", cfg.GeminiModel, "Modelo do Gemini")
	_ = fs.Parse(args)

	if *apiKey != "" {
		cfg.GeminiAPIKey = *apiKey
	}
	if *model != "" {
		cfg.GeminiModel = *model
	}

	ext := extractor.New(cfg, database)
	if err := ext.Run(ctx); err != nil {
		log.Fatalf("[Extractor] Falha na extração: %v", err)
	}
}

func runReprocess(ctx context.Context, cfg *config.Config, database *db.DB, args []string) {
	fs := flag.NewFlagSet("reprocess", flag.ExitOnError)
	apiKey := fs.String("api-key", cfg.GeminiAPIKey, "Chave de API do Gemini")
	model := fs.String("model", cfg.GeminiModel, "Modelo do Gemini")
	_ = fs.Parse(args)

	if *apiKey != "" {
		cfg.GeminiAPIKey = *apiKey
	}
	if *model != "" {
		cfg.GeminiModel = *model
	}

	log.Println("[Reprocess] Limpando eventos anteriores e marcando mensagens como não processadas...")
	_, err := database.ExecContext(ctx, `DELETE FROM events; UPDATE messages SET processed = 0;`)
	if err != nil {
		log.Fatalf("[Reprocess] Falha ao resetar banco: %v", err)
	}

	ext := extractor.New(cfg, database)
	if err := ext.Run(ctx); err != nil {
		log.Fatalf("[Reprocess] Falha na re-extração: %v", err)
	}
}

func runWeb(ctx context.Context, cfg *config.Config, database *db.DB, args []string) {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	port := fs.Int("port", cfg.HTTPPort, "Porta do servidor HTTP")
	_ = fs.Parse(args)

	if *port > 0 {
		cfg.HTTPPort = *port
	}

	srv, err := web.New(cfg, database)
	if err != nil {
		log.Fatalf("[Web] Falha ao inicializar servidor Web: %v", err)
	}

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("[Web] Falha fatal no servidor Web: %v", err)
	}
}

func runAll(ctx context.Context, cfg *config.Config, database *db.DB, args []string) {
	fs := flag.NewFlagSet("all", flag.ExitOnError)
	port := fs.Int("port", cfg.HTTPPort, "Porta do servidor HTTP")
	groupJID := fs.String("group", cfg.WhatsAppGroup, "JID do grupo do WhatsApp")
	_ = fs.Parse(args)

	if *port > 0 {
		cfg.HTTPPort = *port
	}
	if *groupJID != "" {
		cfg.WhatsAppGroup = *groupJID
	}

	errChan := make(chan error, 2)

	// Run Listener in background goroutine
	go func() {
		srv := listener.New(cfg, database)
		if err := srv.Run(ctx); err != nil {
			errChan <- fmt.Errorf("listener erro: %w", err)
		}
	}()

	// Run Web server in background goroutine
	go func() {
		webSrv, err := web.New(cfg, database)
		if err != nil {
			errChan <- fmt.Errorf("web init erro: %w", err)
			return
		}
		if err := webSrv.Run(ctx); err != nil {
			errChan <- fmt.Errorf("web run erro: %w", err)
		}
	}()

	select {
	case err := <-errChan:
		log.Printf("[Main] Erro em serviço: %v", err)
	case <-ctx.Done():
		log.Println("[Main] Encerrando todos os serviços...")
	}
}
