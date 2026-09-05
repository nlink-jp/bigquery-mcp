package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectID != "" || cfg.MaxBytesBilled != DefaultMaxBytesBilled || cfg.JobTimeout != DefaultJobTimeout ||
		cfg.DefaultMaxRows != DefaultDefaultMaxRows || cfg.HardMaxRows != DefaultHardMaxRows || cfg.MaxBytes != DefaultMaxBytes ||
		cfg.LogLevel != DefaultLogLevel || cfg.LogQueries {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadFull(t *testing.T) {
	p := write(t, `
[project]
id = " billing-proj "
location = "asia-northeast1"

[budget]
max_bytes_billed = "2GiB"
job_timeout = "90s"

[access]
datasets = ["proj.ds", "proj2.*", ""]

[results]
default_max_rows = 200
hard_max_rows = 5000
max_bytes = "256KiB"

[logging]
log_file = "/tmp/x.log"
log_level = "debug"
log_queries = true
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectID != "billing-proj" || cfg.Location != "asia-northeast1" {
		t.Errorf("project: %+v", cfg)
	}
	if cfg.MaxBytesBilled != 2<<30 || cfg.JobTimeout != 90*time.Second {
		t.Errorf("budget: %+v", cfg)
	}
	if len(cfg.Datasets) != 2 || cfg.Datasets[0] != "proj.ds" || cfg.Datasets[1] != "proj2.*" {
		t.Errorf("datasets: %v", cfg.Datasets)
	}
	if cfg.DefaultMaxRows != 200 || cfg.HardMaxRows != 5000 || cfg.MaxBytes != 256<<10 {
		t.Errorf("results: %+v", cfg)
	}
	if cfg.LogFile != "/tmp/x.log" || cfg.LogLevel != "debug" || !cfg.LogQueries {
		t.Errorf("logging: %+v", cfg)
	}
	if cfg.Path != p {
		t.Errorf("Path should record the source file")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":          "[budget]\nmax_bytes = \"1GiB\"\n",
		"bad size":             "[budget]\nmax_bytes_billed = \"ten gigs\"\n",
		"zero budget":          "[budget]\nmax_bytes_billed = \"0\"\n",
		"bad duration":         "[budget]\njob_timeout = \"soon\"\n",
		"default above hard":   "[results]\ndefault_max_rows = 10\nhard_max_rows = 5\n",
		"negative rows":        "[results]\ndefault_max_rows = -1\n",
		"wildcard project":     "[access]\ndatasets = [\"*.ds\"]\n",
		"three-part dataset":   "[access]\ndatasets = [\"a.b.c\"]\n",
		"not toml":             "this is not toml = = =\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Errorf("expected an error")
			}
		})
	}
}

func TestUnknownKeyNamesTheKey(t *testing.T) {
	_, err := Load(write(t, "[budget]\nmax_byte_billed = \"1GiB\"\n"))
	if err == nil || !strings.Contains(err.Error(), "max_byte_billed") {
		t.Errorf("error should name the unknown key, got %v", err)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"1048576": 1 << 20, "1KiB": 1 << 10, "10GiB": 10 << 30, "1.5GiB": 3 << 29, "2TB": 2e12,
		"512 MiB": 512 << 20, "1mb": 1e6, "0": 0, "7B": 7,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "GiB", "-1", "x1MiB", "1.2.3GB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 10 << 30: "10.0 GiB", 1536: "1.5 KiB"}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}
