package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// PersistentConfigOptions defines the one-time values used to initialize a
// writable config file on a persistent volume.
type PersistentConfigOptions struct {
	TemplatePath            string
	OutputPath              string
	StateDir                string
	StaticPath              string
	PublicAPIBaseURL        string
	DatabaseURL             string
	BootstrapAdminUsername  string
	BootstrapAdminPassword  string
	JWTSecret               string
	CredentialEncryptionKey string
}

// InitializePersistentConfig creates a complete config file without replacing
// an existing one. Secrets are generated once when the caller does not provide
// them, then remain stable with the persistent file.
func InitializePersistentConfig(options PersistentConfigOptions) error {
	if strings.TrimSpace(options.BootstrapAdminUsername) == "" {
		options.BootstrapAdminUsername = "admin"
	}
	if len(options.BootstrapAdminPassword) < 8 || isExampleSecret(options.BootstrapAdminPassword) {
		return errors.New("首次初始化需要至少 8 个字符的管理员密码")
	}

	template, err := os.ReadFile(options.TemplatePath)
	if err != nil {
		return fmt.Errorf("读取配置模板: %w", err)
	}
	cfg := defaultConfig()
	decoder := yaml.NewDecoder(bytes.NewReader(template))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return fmt.Errorf("解析配置模板: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("解析配置模板: %w", err)
		}
		return errors.New("配置模板只能包含一个 YAML 文档")
	}

	stateDir, err := filepath.Abs(strings.TrimSpace(options.StateDir))
	if err != nil || strings.TrimSpace(options.StateDir) == "" {
		return errors.New("持久化目录无效")
	}
	staticPath, err := filepath.Abs(strings.TrimSpace(options.StaticPath))
	if err != nil || strings.TrimSpace(options.StaticPath) == "" {
		return errors.New("前端静态目录无效")
	}
	outputPath, err := filepath.Abs(strings.TrimSpace(options.OutputPath))
	if err != nil || strings.TrimSpace(options.OutputPath) == "" {
		return errors.New("配置输出路径无效")
	}
	if filepath.Dir(outputPath) != stateDir {
		return errors.New("配置文件必须直接位于持久化目录中")
	}

	jwtSecret := strings.TrimSpace(options.JWTSecret)
	if jwtSecret == "" {
		jwtSecret, err = randomHex(32)
		if err != nil {
			return fmt.Errorf("生成 JWT 密钥: %w", err)
		}
	}
	credentialKey := strings.TrimSpace(options.CredentialEncryptionKey)
	if credentialKey == "" {
		credentialKey, err = randomBase64(32)
		if err != nil {
			return fmt.Errorf("生成凭据加密密钥: %w", err)
		}
	}

	cfg.Server.Listen = "0.0.0.0:8000"
	cfg.Frontend.StaticPath = staticPath
	cfg.Database.SQLite.Path = filepath.Join(stateDir, "backend.db")
	if databaseURL := strings.TrimSpace(options.DatabaseURL); databaseURL != "" {
		cfg.Database.Driver = "postgres"
		cfg.Database.Postgres.DSN = databaseURL
	}
	cfg.Media.Local.Path = filepath.Join(stateDir, "media")
	cfg.Secrets.JWTSecret = jwtSecret
	cfg.Secrets.CredentialEncryptionKey = credentialKey
	cfg.BootstrapAdmin.Username = strings.TrimSpace(options.BootstrapAdminUsername)
	cfg.BootstrapAdmin.Password = options.BootstrapAdminPassword
	if publicURL := strings.TrimRight(strings.TrimSpace(options.PublicAPIBaseURL), "/"); publicURL != "" {
		cfg.Frontend.PublicAPIBaseURL = publicURL
		cfg.Auth.SecureCookies = strings.HasPrefix(strings.ToLower(publicURL), "https://")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("校验初始化配置: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("编码初始化配置: %w", err)
	}
	data = append([]byte("# Generated once by grok2api; keep this file and its secrets persistent.\n"), data...)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("创建持久化目录: %w", err)
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("配置文件已存在: %s", outputPath)
		}
		return fmt.Errorf("创建配置文件: %w", err)
	}
	removeIncomplete := true
	defer func() {
		_ = file.Close()
		if removeIncomplete {
			_ = os.Remove(outputPath)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("写入配置文件: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步配置文件: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭配置文件: %w", err)
	}
	removeIncomplete = false
	return nil
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func randomBase64(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(value), nil
}
