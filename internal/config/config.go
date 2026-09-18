package config

// This package handles the loading and validation of application configuration.
// It reads from environment variables and falls back to default values when necessary.

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all the necessary configuration parameters for the application.
type Config struct {
	Port          int           // HTTP server listening port
	DatabaseURL   string        // PostgreSQL connection string
	MaxWorkers    int           // Maximum number of concurrent workers for checking URLs
	CheckInterval time.Duration // Interval between health checks
	CheckTimeout  time.Duration // Timeout for each individual HTTP check
	DomainRPS     int           // Rate limit (Requests Per Second) per domain
}

// Default configuration values
const (
	defaultPort          = 8080
	defaultMaxWorkers    = 64
	defaultCheckInterval = 30 * time.Second
	defaultCheckTimeout  = 10 * time.Second
	defaultDomainRPS     = 5
)

// Load reads the configuration from the .env file and environment variables,
// sets default values if they are missing, and validates the final configuration.
func Load() (Config, error) {
	// Attempt to load variables from a .env file into the environment.
	// Errors are ignored as the file is optional.
	_ = loadDotEnv(".env")

	// Initialize config with default values.
	cfg := Config{
		MaxWorkers:    defaultMaxWorkers,
		CheckInterval: defaultCheckInterval,
		CheckTimeout:  defaultCheckTimeout,
		DomainRPS:     defaultDomainRPS,
	}

	// Parse PORT environment variable.
	port, err := intEnv("PORT", defaultPort)
	if err != nil {
		return Config{}, err
	}
	cfg.Port = port

	// Read required DATABASE_URL.
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")

	// Parse MAX_WORKERS environment variable.
	maxWorkers, err := intEnv("MAX_WORKERS", defaultMaxWorkers)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxWorkers = maxWorkers

	// Parse CHECK_INTERVAL_SECONDS environment variable.
	interval, err := secondsEnv("CHECK_INTERVAL_SECONDS", defaultCheckInterval)
	if err != nil {
		return Config{}, err
	}
	cfg.CheckInterval = interval

	// Parse CHECK_TIMEOUT_SECONDS environment variable.
	timeout, err := secondsEnv("CHECK_TIMEOUT_SECONDS", defaultCheckTimeout)
	if err != nil {
		return Config{}, err
	}
	cfg.CheckTimeout = timeout

	// Parse DOMAIN_RPS environment variable.
	domainRPS, err := intEnv("DOMAIN_RPS", defaultDomainRPS)
	if err != nil {
		return Config{}, err
	}
	cfg.DomainRPS = domainRPS

	// Validate the loaded configuration.
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate checks if the loaded configuration meets the application's requirements.
func (c Config) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("config: DATABASE_URL is required")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("config: PORT must be between 1 and 65535, got %d", c.Port)
	}
	if c.MaxWorkers < 1 {
		return fmt.Errorf("config: MAX_WORKERS must be at least 1, got %d", c.MaxWorkers)
	}
	if c.CheckInterval <= 0 {
		return fmt.Errorf("config: CHECK_INTERVAL_SECONDS must be positive, got %s", c.CheckInterval)
	}
	if c.CheckTimeout <= 0 {
		return fmt.Errorf("config: CHECK_TIMEOUT_SECONDS must be positive, got %s", c.CheckTimeout)
	}
	if c.DomainRPS < 1 {
		return fmt.Errorf("config: DOMAIN_RPS must be at least 1, got %d", c.DomainRPS)
	}
	return nil
}

// intEnv reads an integer value from an environment variable, returning a default if not found.
func intEnv(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer, got %q", key, raw)
	}
	return v, nil
}

// secondsEnv reads a duration in seconds from an environment variable, returning a default if not found.
func secondsEnv(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer number of seconds, got %q", key, raw)
	}
	return time.Duration(v) * time.Second, nil
}

// loadDotEnv parses a basic .env file and sets the variables into the current environment.
// It handles basic key=value formats and ignores comments or empty lines.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		
		// Remove surrounding quotes if present
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		
		// Only set if not already present in the environment
		if _, ok := os.LookupEnv(key); !ok {
			_ = os.Setenv(key, val)
		}
	}
	return scanner.Err()
}
