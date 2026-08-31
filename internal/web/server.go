package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/takayuki/taiko_info/internal/config"
	"github.com/takayuki/taiko_info/internal/db"
)

//go:embed templates/*
var templateFS embed.FS

// Server encapsulates the HTTP server instance.
type Server struct {
	cfg      *config.Config
	database *db.DB
	tmpl     *template.Template
	loc      *time.Location
}

// PageData represents the view-model for the index template.
type PageData struct {
	UpcomingEvents    []db.Event
	PastEvents        []db.Event
	UpcomingCount     int
	TotalEvents       int
	ProcessedMsgs     int
	CurrentTime       time.Time
	CountAll          int
	CountTreino       int
	CountApresentacao int
	CountGeral        int
}

// New creates and configures the Web Mural server.
func New(cfg *config.Config, database *db.DB) (*Server, error) {
	loc, err := time.LoadLocation(cfg.ReferenceTZ)
	if err != nil {
		loc = time.Local
	}

	funcMap := template.FuncMap{
		"formatCategory": func(cat string) string {
			switch strings.ToLower(strings.TrimSpace(cat)) {
			case "apresentacao", "apresentação":
				return "Apresentação"
			case "treino", "ensaio":
				return "Treino"
			default:
				return "Geral"
			}
		},
		"categoryClass": func(cat string) string {
			switch strings.ToLower(strings.TrimSpace(cat)) {
			case "apresentacao", "apresentação":
				return "cat-apresentacao"
			case "treino", "ensaio":
				return "cat-treino"
			default:
				return "cat-geral"
			}
		},
		"formatMonth": func(t time.Time) string {
			months := []string{"JAN", "FEV", "MAR", "ABR", "MAI", "JUN", "JUL", "AGO", "SET", "OUT", "NOV", "DEZ"}
			return months[t.In(loc).Month()-1]
		},
		"formatDay": func(t time.Time) string {
			return fmt.Sprintf("%02d", t.In(loc).Day())
		},
		"formatWeekday": func(t time.Time) string {
			weekdays := []string{"Domingo", "Segunda", "Terça", "Quarta", "Quinta", "Sexta", "Sábado"}
			return weekdays[t.In(loc).Weekday()]
		},
		"formatTime": func(t time.Time) string {
			return t.In(loc).Format("15:04")
		},
		"formatShortDate": func(t time.Time) string {
			return t.In(loc).Format("02/01/2006 15:04")
		},
		"formatFullDate": func(t time.Time) string {
			return t.In(loc).Format("02/01/2006 15:04:05")
		},
		"formatRelative": func(t time.Time) string {
			now := time.Now().In(loc)
			target := t.In(loc)

			// Same calendar day
			if target.Year() == now.Year() && target.YearDay() == now.YearDay() {
				if target.Before(now.Add(-3 * time.Hour)) {
					return "Concluído"
				}
				if target.Before(now) {
					return "Em andamento"
				}
				return fmt.Sprintf("Hoje às %s", target.Format("15:04"))
			}

			if target.Before(now) {
				return "Concluído"
			}

			// Tomorrow
			tomorrow := now.AddDate(0, 0, 1)
			if target.Year() == tomorrow.Year() && target.YearDay() == tomorrow.YearDay() {
				return fmt.Sprintf("Amanhã às %s", target.Format("15:04"))
			}

			diff := target.Sub(now)
			days := int(diff.Hours() / 24)
			if days <= 0 {
				return fmt.Sprintf("Hoje às %s", target.Format("15:04"))
			} else if days == 1 {
				return "Em 1 dia"
			} else if days < 7 {
				return fmt.Sprintf("Em %d dias", days)
			} else if days < 30 {
				weeks := days / 7
				if weeks == 1 {
					return "Em 1 semana"
				}
				return fmt.Sprintf("Em %d semanas", weeks)
			}
			return fmt.Sprintf("Em %d dias", days)
		},
	}

	tmpl, err := template.New("index.html").Funcs(funcMap).ParseFS(templateFS, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse html templates: %w", err)
	}

	return &Server{
		cfg:      cfg,
		database: database,
		tmpl:     tmpl,
		loc:      loc,
	}, nil
}

// Run starts the native net/http server and handles graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/events", s.handleAPIEvents)
	mux.HandleFunc("/healthz", s.handleHealthz)

	addr := fmt.Sprintf("0.0.0.0:%d", s.cfg.HTTPPort)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErrors := make(chan error, 1)

	go func() {
		log.Printf("[Web] Servidor Mural HTTP escutando em http://localhost:%d", s.cfg.HTTPPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrors <- err
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		return fmt.Errorf("erro no servidor HTTP: %w", err)
	case <-ctx.Done():
		log.Println("[Web] Contexto cancelado, encerrando servidor HTTP...")
	case s := <-sigChan:
		log.Printf("[Web] Sinal recebido (%s), encerrando servidor HTTP...", s)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	events, err := s.database.GetAllEventsInLocation(ctx, s.loc)
	if err != nil {
		http.Error(w, fmt.Sprintf("Erro ao consultar eventos: %v", err), http.StatusInternalServerError)
		return
	}

	totalEvents, _, processedMsgs, _ := s.database.GetStats(ctx)

	now := time.Now().In(s.loc)
	var upcoming []db.Event
	var past []db.Event
	var countTreino, countApresentacao, countGeral int

	for _, ev := range events {
		switch strings.ToLower(strings.TrimSpace(ev.Category)) {
		case "apresentacao", "apresentação":
			countApresentacao++
		case "treino", "ensaio":
			countTreino++
		default:
			countGeral++
		}

		evTime := ev.EventDate.In(s.loc)
		if evTime.After(now.Add(-2 * time.Hour)) {
			upcoming = append(upcoming, ev)
		} else {
			past = append(past, ev)
		}
	}

	// Upcoming events: closest/nearest date first (e.g. today -> tomorrow -> next month)
	sort.Slice(upcoming, func(i, j int) bool {
		return upcoming[i].EventDate.Before(upcoming[j].EventDate)
	})

	// Past events: most recently finished first (e.g. yesterday -> last week)
	sort.Slice(past, func(i, j int) bool {
		return past[i].EventDate.After(past[j].EventDate)
	})

	data := PageData{
		UpcomingEvents:    upcoming,
		PastEvents:        past,
		UpcomingCount:     len(upcoming),
		TotalEvents:       totalEvents,
		ProcessedMsgs:     processedMsgs,
		CurrentTime:       now,
		CountAll:          len(events),
		CountTreino:       countTreino,
		CountApresentacao: countApresentacao,
		CountGeral:        countGeral,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("[Web] Erro ao renderizar template: %v", err)
	}
}

func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	events, err := s.database.GetAllEventsInLocation(ctx, s.loc)
	if err != nil {
		http.Error(w, `{"error": "falha ao buscar eventos"}`, http.StatusInternalServerError)
		return
	}

	if events == nil {
		events = []db.Event{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(events)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
