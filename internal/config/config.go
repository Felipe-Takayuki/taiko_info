package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Config holds the runtime configuration for all modules.
type Config struct {
	DBPath         string
	WhatsAppGroup  string
	GeminiAPIKey   string
	GeminiModel    string
	HTTPPort       int
	LogLevel       string
	ReferenceTZ    string
}

// Load loads configuration from environment variables, falling back to defaults.
// It also parses a local .env file if available.
func Load() *Config {
	loadDotEnv(".env")

	port, err := strconv.Atoi(getEnv("HTTP_PORT", getEnv("PORT", "8080")))
	if err != nil || port <= 0 {
		port = 8080
	}

	model := getEnv("GEMINI_MODEL", "gemini-1.5-flash")
	if model == "" {
		model = "gemini-1.5-flash"
	}

	return &Config{
		DBPath:        getEnv("DB_PATH", "app.db"),
		WhatsAppGroup: getEnv("WHATSAPP_GROUP_JID", ""),
		GeminiAPIKey:  getEnv("GEMINI_API_KEY", ""),
		GeminiModel:   model,
		HTTPPort:      port,
		LogLevel:      getEnv("LOG_LEVEL", "INFO"),
		ReferenceTZ:   getEnv("TIMEZONE", "America/Sao_Paulo"),
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return strings.TrimSpace(val)
	}
	return defaultVal
}

// loadDotEnv parses key=value pairs from a .env file and sets them into os.Environ if not already set.
func loadDotEnv(filepath string) {
	file, err := os.Open(filepath)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		// Strip quotes if present
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}

		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}
