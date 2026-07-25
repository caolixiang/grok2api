package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/chenyme/grok2api/backend/internal/app"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/observability"
)

// Run 解析启动参数并运行后端服务。
func Run(args []string) error {
	if len(args) > 0 && args[0] == "init-config" {
		return initializeConfig(args[1:])
	}
	options, err := parseOptions(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(options.configPath)
	if err != nil {
		return err
	}
	if options.listen != "" {
		cfg.Server.Listen = options.listen
		if err := cfg.Validate(); err != nil {
			return err
		}
	}
	logger := observability.NewLogger()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer application.Close()
	return application.Run(ctx)
}

func initializeConfig(args []string) error {
	flags := flag.NewFlagSet("init-config", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	templatePath := flags.String("template", "", "configuration template")
	outputPath := flags.String("output", "", "persistent configuration output")
	stateDir := flags.String("state-dir", "", "persistent state directory")
	staticPath := flags.String("static-path", "", "frontend static directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *templatePath == "" || *outputPath == "" || *stateDir == "" || *staticPath == "" {
		return errors.New("init-config 需要 --template、--output、--state-dir 和 --static-path")
	}
	publicURL := strings.TrimSpace(os.Getenv("GROK2API_PUBLIC_API_BASE_URL"))
	if publicURL == "" {
		if domain := strings.TrimSpace(os.Getenv("RAILWAY_PUBLIC_DOMAIN")); domain != "" {
			publicURL = "https://" + domain
		}
	}
	return config.InitializePersistentConfig(config.PersistentConfigOptions{
		TemplatePath: *templatePath, OutputPath: *outputPath, StateDir: *stateDir, StaticPath: *staticPath,
		PublicAPIBaseURL:        publicURL,
		DatabaseURL:             firstEnvironment("GROK2API_DATABASE_URL", "DATABASE_URL"),
		BootstrapAdminUsername:  strings.TrimSpace(os.Getenv("GROK2API_BOOTSTRAP_ADMIN_USERNAME")),
		BootstrapAdminPassword:  os.Getenv("GROK2API_BOOTSTRAP_ADMIN_PASSWORD"),
		JWTSecret:               strings.TrimSpace(os.Getenv("GROK2API_JWT_SECRET")),
		CredentialEncryptionKey: strings.TrimSpace(os.Getenv("GROK2API_CREDENTIAL_ENCRYPTION_KEY")),
	})
}

func firstEnvironment(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

type runOptions struct {
	configPath string
	listen     string
}

func parseOptions(args []string) (runOptions, error) {
	options := runOptions{configPath: defaultConfigPath()}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--config":
			if index+1 >= len(args) {
				return runOptions{}, errors.New("--config 缺少路径")
			}
			options.configPath = args[index+1]
			index++
		case "--listen":
			if index+1 >= len(args) {
				return runOptions{}, errors.New("--listen 缺少地址")
			}
			options.listen = args[index+1]
			index++
		default:
			return runOptions{}, fmt.Errorf("不支持的启动参数: %s", args[index])
		}
	}
	return options, nil
}

func defaultConfigPath() string {
	for _, candidate := range []string{"config.yaml", filepath.Join("..", "config.yaml")} {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate
		}
	}
	return "config.yaml"
}
