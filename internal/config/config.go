package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port          int
	DatabaseURL   string
	MaxWorkers    int
	CheckInterval time.Duration
	CheckTimeout  time.Duration
	DomainRPS     int
}

const (
	defaultPort          = 8080
	defaultMaxWorkers    = 64
	defaultCheckInterval = 30 * time.Second
	defaultCheckTimeout  = 10 * time.Second
	defaultDomainRPS     = 5
)

func Load() (Config, error) {
	_ = loadDotEnv(".env")

	cfg := Config{
		MaxWorkers:    defaultMaxWorkers,
		CheckInterval: defaultCheckInterval,
		CheckTimeout:  defaultCheckTimeout,
		DomainRPS:     defaultDomainRPS,
	}

	port, err := intEnv("PORT", defaultPort)
	if err != nil {
		return Config{}, err
	}
	cfg.Port = port

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")

	maxWorkers, err := intEnv("MAX_WORKERS", defaultMaxWorkers)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxWorkers = maxWorkers

	interval, err := secondsEnv("CHECK_INTERVAL_SECONDS", defaultCheckInterval)
	if err != nil {
		return Config{}, err
	}
	cfg.CheckInterval = interval

	timeout, err := secondsEnv("CHECK_TIMEOUT_SECONDS", defaultCheckTimeout)
	if err != nil {
		return Config{}, err
	}
	cfg.CheckTimeout = timeout

	domainRPS, err := intEnv("DOMAIN_RPS", defaultDomainRPS)
	if err != nil {
		return Config{}, err
	}
	cfg.DomainRPS = domainRPS

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

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

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if _, ok := os.LookupEnv(key); !ok {
			_ = os.Setenv(key, val)
		}
	}
	return scanner.Err()
}
