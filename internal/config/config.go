// Package config loads bigquery-mcp's runtime configuration.
//
// One server instance serves exactly one billing project (ADR-0002 in the
// RFP's terms: the model must never choose where a query is billed). Several
// projects are handled by registering the binary several times with
// different --config paths — there is deliberately no profile mechanism.
//
// The file holds no credentials: authentication is Application Default
// Credentials, read by the bq package.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults. Every value is overridable in the file.
const (
	DefaultMaxBytesBilled int64 = 10 << 30 // 10 GiB
	DefaultJobTimeout           = 3 * time.Minute
	DefaultDefaultMaxRows       = 1000
	DefaultHardMaxRows          = 50000
	DefaultMaxBytes       int64 = 1 << 20 // 1 MiB response budget
	DefaultLogLevel             = "info"
	// MinBytesBilled is BigQuery's per-table billing minimum; a budget
	// below it fails every real query (review finding 8).
	MinBytesBilled int64 = 10 << 20
)

// Config is the resolved configuration.
type Config struct {
	// [project]
	ProjectID string
	Location  string

	// [budget]
	MaxBytesBilled int64
	JobTimeout     time.Duration

	// [access]
	Datasets []string

	// [results]
	DefaultMaxRows int
	HardMaxRows    int
	MaxBytes       int64

	// [logging]
	LogFile    string
	LogLevel   string
	LogQueries bool

	// Path is the file the values came from ("" when defaults only).
	Path string
}

type tomlConfig struct {
	Project tomlProject `toml:"project"`
	Budget  tomlBudget  `toml:"budget"`
	Access  tomlAccess  `toml:"access"`
	Results tomlResults `toml:"results"`
	Logging tomlLogging `toml:"logging"`
}

type tomlProject struct {
	ID       string `toml:"id"`
	Location string `toml:"location"`
}

type tomlBudget struct {
	MaxBytesBilled string `toml:"max_bytes_billed"`
	JobTimeout     string `toml:"job_timeout"`
}

type tomlAccess struct {
	Datasets []string `toml:"datasets"`
}

type tomlResults struct {
	DefaultMaxRows *int   `toml:"default_max_rows"`
	HardMaxRows    *int   `toml:"hard_max_rows"`
	MaxBytes       string `toml:"max_bytes"`
}

type tomlLogging struct {
	LogFile    string `toml:"log_file"`
	LogLevel   string `toml:"log_level"`
	LogQueries bool   `toml:"log_queries"`
}

// DefaultPath returns ~/.config/bigquery-mcp/config.toml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "bigquery-mcp", "config.toml")
}

// Default returns the defaults with no project set.
func Default() *Config {
	return &Config{
		MaxBytesBilled: DefaultMaxBytesBilled,
		JobTimeout:     DefaultJobTimeout,
		DefaultMaxRows: DefaultDefaultMaxRows,
		HardMaxRows:    DefaultHardMaxRows,
		MaxBytes:       DefaultMaxBytes,
		LogLevel:       DefaultLogLevel,
	}
}

// Load reads the TOML file at path. A missing file at the default path is
// not an error — the returned Config holds the defaults (and serve refuses
// to start without a project id) — but a path the operator named must
// exist. Unknown keys are an error: a misspelled budget key must not
// silently leave the default in place.
func Load(path string) (*Config, error) {
	return load(path, path != "")
}

// LoadDefault reads the default path, tolerating its absence.
func LoadDefault() (*Config, error) { return load(DefaultPath(), false) }

func load(path string, mustExist bool) (*Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath()
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if mustExist {
				return nil, fmt.Errorf("config: %s does not exist", path)
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("config: stat %s: %w", path, err)
	}
	var raw tomlConfig
	meta, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("config: unknown key(s) in %s: %s", path, strings.Join(keys, ", "))
	}
	if err := apply(cfg, &raw); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	cfg.Path = path
	return cfg, nil
}

func apply(cfg *Config, raw *tomlConfig) error {
	cfg.ProjectID = strings.TrimSpace(raw.Project.ID)
	cfg.Location = strings.TrimSpace(raw.Project.Location)

	if s := strings.TrimSpace(raw.Budget.MaxBytesBilled); s != "" {
		n, err := ParseSize(s)
		if err != nil {
			return fmt.Errorf("[budget] max_bytes_billed: %w", err)
		}
		if n < MinBytesBilled {
			return fmt.Errorf("[budget] max_bytes_billed must be at least 10MiB (BigQuery bills 10 MB per table at minimum), got %s", s)
		}
		cfg.MaxBytesBilled = n
	}
	if s := strings.TrimSpace(raw.Budget.JobTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("[budget] job_timeout: %w", err)
		}
		if d <= 0 {
			return errors.New("[budget] job_timeout must be positive")
		}
		cfg.JobTimeout = d
	}

	for _, d := range raw.Access.Datasets {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if err := validateDatasetPattern(d); err != nil {
			return fmt.Errorf("[access] datasets: %w", err)
		}
		cfg.Datasets = append(cfg.Datasets, d)
	}

	if v := raw.Results.DefaultMaxRows; v != nil {
		if *v <= 0 {
			return errors.New("[results] default_max_rows must be positive")
		}
		cfg.DefaultMaxRows = *v
	}
	if v := raw.Results.HardMaxRows; v != nil {
		if *v <= 0 {
			return errors.New("[results] hard_max_rows must be positive")
		}
		cfg.HardMaxRows = *v
	}
	if cfg.DefaultMaxRows > cfg.HardMaxRows {
		return fmt.Errorf("[results] default_max_rows (%d) must not exceed hard_max_rows (%d)", cfg.DefaultMaxRows, cfg.HardMaxRows)
	}
	if s := strings.TrimSpace(raw.Results.MaxBytes); s != "" {
		n, err := ParseSize(s)
		if err != nil {
			return fmt.Errorf("[results] max_bytes: %w", err)
		}
		if n <= 0 {
			return errors.New("[results] max_bytes must be positive")
		}
		cfg.MaxBytes = n
	}

	cfg.LogFile = strings.TrimSpace(raw.Logging.LogFile)
	if s := strings.ToLower(strings.TrimSpace(raw.Logging.LogLevel)); s != "" {
		switch s {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = s
		default:
			return fmt.Errorf("[logging] log_level must be debug, info, warn or error, got %q", s)
		}
	}
	cfg.LogQueries = raw.Logging.LogQueries
	return nil
}

// validateDatasetPattern accepts "project.dataset" or "project.*". The
// dataset is what follows the last dot, so a domain-scoped project id
// ("example.com:proj") keeps its own dot.
func validateDatasetPattern(p string) error {
	project, dataset, ok := SplitDataset(p)
	if !ok {
		return fmt.Errorf("%q is not project.dataset or project.*", p)
	}
	if strings.Contains(project, "*") {
		return fmt.Errorf("%q: the project part may not be a wildcard", p)
	}
	if strings.Contains(dataset, "*") && dataset != "*" {
		return fmt.Errorf("%q: the dataset part is either a name or exactly *", p)
	}
	return nil
}

// SplitDataset splits "project.dataset" at the last dot.
func SplitDataset(p string) (project, dataset string, ok bool) {
	i := strings.LastIndex(p, ".")
	if i <= 0 || i == len(p)-1 {
		return "", "", false
	}
	return p[:i], p[i+1:], true
}

// ParseSize parses a byte size such as "10GiB", "512MiB", "1.5GB", "1048576".
// Binary suffixes (KiB, MiB, GiB, TiB) are powers of 1024; decimal ones
// (KB, MB, GB, TB) are powers of 1000; a bare number is bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	units := []struct {
		suffix string
		mult   float64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
	}
	lower := strings.ToLower(s)
	for _, u := range units {
		if strings.HasSuffix(lower, strings.ToLower(u.suffix)) {
			num := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			v := f * u.mult
			if v < 0 || v > math.MaxInt64 {
				return 0, fmt.Errorf("size %q out of range", s)
			}
			return int64(v), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n, nil
}

// FormatSize renders bytes with a binary suffix for messages ("10.0 GiB").
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
