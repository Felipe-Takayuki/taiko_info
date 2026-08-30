# Ryuka Taiko Info - Gerenciador Autônomo de Eventos WhatsApp

Sistema autônomo, robusto e de altíssimo desempenho desenvolvido em **100% Go (Golang)**, **SQLite** e **Google Gemini**, projetado para rodar em servidores de baixo consumo de recursos (Raspberry Pi, VPS com 512MB RAM, instâncias micro).

O sistema escuta mensagens de um grupo específico do WhatsApp, armazena o histórico em um banco SQLite com modo WAL, extrai compromissos/ensaios/eventos usando LLM e disponibiliza um mural web responsivo e elegante com a identidade visual **Ryuka (Sousaku Eisa Taiko)** em Dark Mode.

---

## Visão Geral da Arquitetura

```
                     ┌──────────────────────────────────────────────┐
                     │          WhatsApp (Grupo Alvo)               │
                     └──────────────────────┬───────────────────────┘
                                            │ (whatsmeow daemon)
                                            ▼
                     ┌──────────────────────────────────────────────┐
                     │          Módulo 1: Listener Daemon           │
                     │         - QR Code Auth no Terminal           │
                     │         - Reconexão Automática               │
                     │         - Filtro por GroupJID                │
                     └──────────────────────┬───────────────────────┘
                                            │ INSERT
                                            ▼
       ┌────────────────────────────────────────────────────────────────────────┐
       │                   SQLite Local (app.db)                                │
       │   - Concorrência Segura: WAL mode + busy_timeout=5000 + conn pooling   │
       │   - messages (id, sender, message, created_at, processed)              │
       │   - events (id, title, description, event_date, source_sender, ...)    │
       └──────────────┬──────────────────────────────────────────┬──────────────┘
                      │ (SELECT WHERE processed = 0)             │ (SELECT events)
                      ▼                                          ▼
┌──────────────────────────────────────────────┐ ┌──────────────────────────────┐
│        Módulo 2: Extrator Semanal            │ │    Módulo 3: Mural Web       │
│  - Leitura de mensagens pendentes            │ │  - Go net/http Nativo        │
│  - Chamada REST à LLM (Google Gemini)        │ │  - html/template com embed   │
│  - Extração com schema JSON estrito          │ │  - UI Responsiva Dark Mode   │
│  - Transação Atômica: INSERT + processed=1   │ │  - Endpoint JSON & Healthz   │
└──────────────────────────────────────────────┘ └──────────────────────────────┘
```

---

## Estrutura de Diretórios

```
.
├── cmd/
│   └── taiko/
│       └── main.go                 # Ponto de entrada CLI (subcomandos)
├── internal/
│   ├── config/
│   │   └── config.go               # Leitor de variáveis de ambiente e .env
│   ├── db/
│   │   ├── db.go                   # Camada SQLite, migrações e operações atômicas
│   │   └── db_test.go              # Testes unitários do banco de dados
│   ├── extractor/
│   │   ├── extractor.go            # Integração REST com Gemini LLM e transações
│   │   └── extractor_test.go       # Testes unitários de parsing de data e JSON
│   ├── listener/
│   │   └── listener.go             # Whatsmeow, autenticação QR Code e captura
│   └── web/
│       ├── server.go               # Servidor net/http nativo com templates embutidos
│       ├── server_test.go          # Testes unitários do servidor web
│       └── templates/
│           └── index.html          # Template HTML5/CSS3 Dark Mode
├── systemd/
│   ├── taiko-listener.service      # Serviço do daemon do WhatsApp
│   ├── taiko-web.service           # Serviço do servidor web
│   ├── taiko-extractor.service     # Serviço de extração em lote
│   └── taiko-extractor.timer       # Timer semanal do Systemd
├── .env.example                    # Exemplo de configuração de variáveis
├── go.mod                          # Módulos Go
├── go.sum                          # Checksums de dependências
├── main.go                         # Atalho de entrada para go run .
└── README.md                       # Documentação completa
```

---

## Esquema do Banco de Dados (app.db)

O banco é inicializado automaticamente na primeira execução com pragmas de segurança e desempenho:

```sql
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = 1;

CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    sender TEXT,
    message TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    processed INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_messages_processed ON messages(processed);

CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT NOT NULL,
    description TEXT,
    event_date DATETIME NOT NULL,
    source_sender TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_events_date ON events(event_date);
```

---

## Configuração (.env)

Crie seu arquivo `.env` baseado no modelo:

```bash
cp .env.example .env
```

Edite o arquivo `.env`:

```ini
# Caminho do banco SQLite
DB_PATH=app.db

# JID do Grupo do WhatsApp (ex: 120363048999999999@g.us)
# Deixe vazio na primeira inicialização para visualizar o JID nos logs
WHATSAPP_GROUP_JID=120363048999999999@g.us

# Chave da API do Google Gemini (gratuita em https://aistudio.google.com/)
GEMINI_API_KEY=AIzaSy...

# Modelo Gemini (padrão: gemini-1.5-flash)
GEMINI_MODEL=gemini-1.5-flash

# Porta do Mural Web
HTTP_PORT=8080

# Fuso horário para resolução de datas relativas (ex: "hoje às 19h", "próximo sábado")
TIMEZONE=America/Sao_Paulo

# Nível de log (INFO ou DEBUG)
LOG_LEVEL=INFO
```

---

## Como Compilar e Executar

### 1. Compilação (Binário único sem frameworks pesados)

```bash
go build -o taiko .
```

### 2. Autenticação e Execução do WhatsApp Listener (Módulo 1)

Na primeira execução, o daemon exibirá um **QR Code em blocos UTF-8 diretamente no seu terminal**:

```bash
./taiko listener
```

1. Abra o WhatsApp no seu smartphone.
2. Vá em **Aparelhos Conectados** > **Conectar um aparelho**.
3. Aponte a câmera para o QR Code no terminal.
4. A sessão será salva automaticamente no arquivo `app.db` e restaurada nas próximas inicializações sem necessidade de novo scan.

### 3. Execução Manual do Extrator LLM (Módulo 2)

```bash
./taiko extractor
```

*O comando busca todas as mensagens com `processed = 0`, submete o histórico concatenado para a API do Gemini com restrição de schema JSON, insere os eventos na tabela `events` e marca as mensagens com `processed = 1` de forma transacional e atômica.*

### 4. Execução do Mural Web (Módulo 3)

```bash
./taiko web -port=8080
```

Acesse no navegador: **`http://localhost:8080`**

### 5. Execução Unificada (Listener + Web simultaneamente)

```bash
./taiko all
```

---

## Agendamento Periódico (Semanal / Diário)

### Opção A: Usando Systemd Timers (Recomendado no Linux moderno)

Copie os arquivos de serviço para o diretório de serviços do sistema ou do usuário:

```bash
# Copiar arquivos de serviço e timer
sudo cp systemd/taiko-listener.service /etc/systemd/system/
sudo cp systemd/taiko-web.service /etc/systemd/system/
sudo cp systemd/taiko-extractor.service /etc/systemd/system/
sudo cp systemd/taiko-extractor.timer /etc/systemd/system/

# Recarregar o daemon do systemd
sudo systemctl daemon-reload

# Habilitar e iniciar os serviços em segundo plano
sudo systemctl enable --now taiko-listener.service
sudo systemctl enable --now taiko-web.service
sudo systemctl enable --now taiko-extractor.timer
```

Para verificar o status do agendador semanal:
```bash
systemctl list-timers | grep taiko
```

### Opção B: Usando Crontab Tradicional

Adicione uma linha ao crontab (`crontab -e`) para executar todo domingo às 03:00 da manhã:

```cron
# Extração semanal de eventos todo domingo às 03:00 da manhã
0 3 * * 0 cd /home/takayuki/Projects/taiko_info && ./taiko extractor >> extractor.log 2>&1
```

---

## Testes Automatizados

Para rodar todos os testes de banco de dados, parsing de LLM e renderização HTTP:

```bash
go test -v ./...
```

---

## Segurança e Eficiência de Recursos

- **Zero Alocação Excessiva**: Binário Go compilado consome menos de 25MB de RAM em execução estável.
- **SQLite Concorrência Segura**: Configurado com `WAL` (Write-Ahead Logging), `busy_timeout=5000` e pool serializado (`SetMaxOpenConns(1)` / transações atômicas com `BeginTx`) prevenindo deadlocks (`SQLITE_BUSY`).
- **Templates Nativos com `embed`**: CSS e HTML embutidos diretamente no binário (`//go:embed`), sem requisições a CDNs externas ou arquivos externos perdidos.
- **Graceful Shutdown**: Encerramento limpo capturando `SIGINT` e `SIGTERM` com desconexão correta do cliente WhatsApp e flush das transações do banco.
