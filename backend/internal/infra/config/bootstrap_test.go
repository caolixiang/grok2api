package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitializePersistentConfigCreatesStableVolumeConfig(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	templatePath := filepath.Join(root, "config.example.yaml")
	template, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(templatePath, template, 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(stateDir, "config.yaml")
	staticPath := filepath.Join(root, "frontend", "dist")
	options := PersistentConfigOptions{
		TemplatePath: templatePath, OutputPath: outputPath, StateDir: stateDir, StaticPath: staticPath,
		PublicAPIBaseURL: "https://grok.example/", BootstrapAdminUsername: "owner", BootstrapAdminPassword: "password123",
	}
	if err := InitializePersistentConfig(options); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.SQLite.Path != filepath.Join(stateDir, "backend.db") || cfg.Media.Local.Path != filepath.Join(stateDir, "media") {
		t.Fatalf("persistent paths: database=%q media=%q", cfg.Database.SQLite.Path, cfg.Media.Local.Path)
	}
	if cfg.Frontend.StaticPath != staticPath || cfg.Frontend.PublicAPIBaseURL != "https://grok.example" {
		t.Fatalf("frontend config = %#v", cfg.Frontend)
	}
	if !cfg.Auth.SecureCookies {
		t.Fatal("HTTPS public URL did not enable secure cookies")
	}
	if len(cfg.Secrets.JWTSecret) != 64 || !validCredentialEncryptionKey(cfg.Secrets.CredentialEncryptionKey) {
		t.Fatalf("generated secrets are invalid: jwt=%d credential=%q", len(cfg.Secrets.JWTSecret), cfg.Secrets.CredentialEncryptionKey)
	}
	if cfg.BootstrapAdmin.Username != "owner" || cfg.BootstrapAdmin.Password != "password123" {
		t.Fatalf("bootstrap admin = %#v", cfg.BootstrapAdmin)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	if err := InitializePersistentConfig(options); err == nil {
		t.Fatal("existing persistent config was overwritten")
	}
}

func TestInitializePersistentConfigRequiresBootstrapPassword(t *testing.T) {
	root := t.TempDir()
	err := InitializePersistentConfig(PersistentConfigOptions{
		TemplatePath: filepath.Join(root, "missing.yaml"), OutputPath: filepath.Join(root, "config.yaml"),
		StateDir: root, StaticPath: root,
	})
	if err == nil {
		t.Fatal("missing bootstrap password was accepted")
	}
}

func TestInitializePersistentConfigUsesPostgresDatabaseURL(t *testing.T) {
	root := t.TempDir()
	templatePath := filepath.Join(root, "config.example.yaml")
	template, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(templatePath, template, 0o600); err != nil {
		t.Fatal(err)
	}
	dsn := "postgres://user:password@postgres.railway.internal:5432/railway"
	outputPath := filepath.Join(root, "config.yaml")
	if err := InitializePersistentConfig(PersistentConfigOptions{
		TemplatePath: templatePath, OutputPath: outputPath, StateDir: root, StaticPath: root,
		DatabaseURL: dsn, BootstrapAdminPassword: "password123",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Driver != "postgres" || cfg.Database.Postgres.DSN != dsn {
		t.Fatalf("database config = %#v", cfg.Database)
	}
}
