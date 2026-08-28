package listener

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/takayuki/taiko_info/internal/config"
	"github.com/takayuki/taiko_info/internal/db"
)

// Listener manages the WhatsApp connection and message ingestion.
type Listener struct {
	cfg      *config.Config
	database *db.DB
	client   *whatsmeow.Client
	waLogger waLog.Logger
}

// New creates a new WhatsApp listener service.
func New(cfg *config.Config, database *db.DB) *Listener {
	var logger waLog.Logger
	if strings.ToUpper(cfg.LogLevel) == "DEBUG" {
		logger = waLog.Stdout("WhatsApp", "DEBUG", true)
	} else {
		logger = waLog.Stdout("WhatsApp", "INFO", true)
	}

	return &Listener{
		cfg:      cfg,
		database: database,
		waLogger: logger,
	}
}

// Run starts the listener daemon, performs QR authentication if needed, and listens for messages.
func (l *Listener) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", l.cfg.DBPath)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, l.waLogger)
	if err != nil {
		return fmt.Errorf("failed to initialize whatsmeow store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("failed to get WhatsApp device store: %w", err)
	}

	client := whatsmeow.NewClient(deviceStore, l.waLogger)
	l.client = client

	// Register event handler
	client.AddEventHandler(l.handleEvent)

	// Authentication handling
	if client.Store.ID == nil {
		// New login: generate QR code in terminal
		qrChan, err := client.GetQRChannel(ctx)
		if err != nil {
			return fmt.Errorf("failed to get QR code channel: %w", err)
		}

		if err = client.Connect(); err != nil {
			return fmt.Errorf("failed to connect WhatsApp client: %w", err)
		}

		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					fmt.Println("\n================================================================")
					fmt.Println("       ESCANEIE O QR CODE COM SEU APLICATIVO DO WHATSAPP        ")
					fmt.Println("================================================================")
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
					fmt.Println("================================================================")
				} else {
					log.Printf("[Listener] Evento QR recebido: %s", evt.Event)
				}
			}
		}()
	} else {
		// Already paired: connect directly
		if err = client.Connect(); err != nil {
			return fmt.Errorf("failed to connect with existing session: %w", err)
		}
		log.Println("[Listener] Sessão WhatsApp restaurada com sucesso.")
	}

	log.Printf("[Listener] Escutando mensagens... Filtro de Grupo: %s", func() string {
		if l.cfg.WhatsAppGroup != "" {
			return l.cfg.WhatsAppGroup
		}
		return "(TODOS - configure WHATSAPP_GROUP_JID para restringir)"
	}())

	// Wait for OS termination signal or context cancellation
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-ctx.Done():
		log.Println("[Listener] Contexto cancelado, encerrando...")
	case s := <-sigChan:
		log.Printf("[Listener] Sinal recebido (%s), desconectando...", s)
	}

	l.client.Disconnect()
	return nil
}

// handleEvent processes incoming WhatsApp events.
func (l *Listener) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		log.Println("[Listener] Conexão estabelecida com os servidores do WhatsApp.")

	case *events.LoggedOut:
		log.Println("[Listener] Sessão desconectada pelo usuário no celular.")

	case *events.Message:
		l.processMessage(v)
	}
}

// processMessage extracts and persists relevant text messages.
func (l *Listener) processMessage(msg *events.Message) {
	chatJID := msg.Info.Chat.String()
	chatUser := msg.Info.Chat.User

	// Check if group filter is active
	if l.cfg.WhatsAppGroup != "" {
		target := strings.TrimSpace(l.cfg.WhatsAppGroup)
		isMatching := chatJID == target ||
			chatUser == target ||
			strings.HasPrefix(chatJID, target+"@") ||
			(strings.HasSuffix(target, "@g.us") && chatJID == target)

		if !isMatching {
			// Ignored because it doesn't match configured group
			return
		}
	} else if !msg.Info.IsGroup {
		// If no filter is set, only log non-group chat info to avoid noise
		return
	}

	text := extractMessageText(msg)
	if strings.TrimSpace(text) == "" {
		// Ignore empty or non-textual messages (reactions, stickers without caption, audio, etc.)
		return
	}

	senderName := msg.Info.PushName
	if senderName == "" {
		senderName = msg.Info.Sender.User
	}
	senderDisplay := fmt.Sprintf("%s (%s)", senderName, msg.Info.Sender.User)
	if senderName == msg.Info.Sender.User {
		senderDisplay = senderName
	}

	timestamp := msg.Info.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := l.database.SaveMessage(ctx, msg.Info.ID, senderDisplay, text, timestamp)
	if err != nil {
		log.Printf("[Listener] Erro ao salvar mensagem %s: %v", msg.Info.ID, err)
		return
	}

	log.Printf("[Listener] Mensagem salva [%s] de '%s' no chat '%s': %.60s...",
		msg.Info.ID, senderDisplay, chatJID, strings.ReplaceAll(text, "\n", " "))
}

// extractMessageText extracts textual content from different message envelope types.
func extractMessageText(msg *events.Message) string {
	if msg == nil || msg.Message == nil {
		return ""
	}

	m := msg.Message

	if text := m.GetConversation(); text != "" {
		return text
	}
	if ext := m.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" {
		return ext.GetText()
	}
	if img := m.GetImageMessage(); img != nil && img.GetCaption() != "" {
		return img.GetCaption()
	}
	if vid := m.GetVideoMessage(); vid != nil && vid.GetCaption() != "" {
		return vid.GetCaption()
	}
	if doc := m.GetDocumentMessage(); doc != nil && doc.GetCaption() != "" {
		return doc.GetCaption()
	}

	return ""
}

// ListGroups connects to WhatsApp using the stored session and lists all joined groups with their JIDs.
func (l *Listener) ListGroups(ctx context.Context) error {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", l.cfg.DBPath)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, l.waLogger)
	if err != nil {
		return fmt.Errorf("falha ao inicializar whatsmeow store: %w", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("falha ao obter device store: %w", err)
	}

	if deviceStore.ID == nil {
		return fmt.Errorf("nenhuma sessão WhatsApp encontrada. Execute './taiko listener' primeiro para parear via QR code")
	}

	client := whatsmeow.NewClient(deviceStore, l.waLogger)
	if err = client.Connect(); err != nil {
		return fmt.Errorf("falha ao conectar ao WhatsApp: %w", err)
	}
	defer client.Disconnect()

	// Wait briefly for handshake
	time.Sleep(2 * time.Second)

	groups, err := client.GetJoinedGroups(ctx)
	if err != nil {
		return fmt.Errorf("falha ao obter lista de grupos: %w", err)
	}

	if len(groups) == 0 {
		fmt.Println("\nNenhum grupo encontrado nesta conta do WhatsApp.")
		return nil
	}

	fmt.Printf("\n===============================================================================\n")
	fmt.Printf("           LISTA DE GRUPOS ENCONTRADOS (%d grupos)                             \n", len(groups))
	fmt.Printf("===============================================================================\n\n")

	for i, g := range groups {
		adminTag := ""
		if g.IsAnnounce {
			adminTag = " (🔒 Apenas Administradores podem enviar mensagens)"
		}
		fmt.Printf("[%2d] %s%s\n     JID: %s\n\n", i+1, g.Name, adminTag, g.JID.String())
	}

	fmt.Printf("===============================================================================\n")
	fmt.Printf("Copie o JID do grupo desejado e cole no seu .env: WHATSAPP_GROUP_JID=<JID>\n")
	fmt.Printf("===============================================================================\n")
	return nil
}

