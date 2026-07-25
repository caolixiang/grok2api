package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigPathFindsRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	backendDir := filepath.Join(root, "backend")
	if err := os.Mkdir(backendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("server: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(backendDir)

	if path := defaultConfigPath(); path != filepath.Join("..", "config.yaml") {
		t.Fatalf("defaultConfigPath() = %q", path)
	}
}

func TestParseOptionsSupportsContainerListenOverride(t *testing.T) {
	options, err := parseOptions([]string{"--config", "/app/config.yaml", "--listen", "0.0.0.0:8000"})
	if err != nil {
		t.Fatal(err)
	}
	if options.configPath != "/app/config.yaml" || options.listen != "0.0.0.0:8000" {
		t.Fatalf("options = %#v", options)
	}
	if _, err := parseOptions([]string{"--listen"}); err == nil {
		t.Fatal("missing --listen value was accepted")
	}
}

func TestFirstEnvironmentUsesExplicitDatabaseOverride(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://railway")
	t.Setenv("GROK2API_DATABASE_URL", "postgres://explicit")
	if value := firstEnvironment("GROK2API_DATABASE_URL", "DATABASE_URL"); value != "postgres://explicit" {
		t.Fatalf("database URL = %q", value)
	}
}
