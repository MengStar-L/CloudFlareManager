package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cf-r2-manager/cf-r2-manager/internal/app"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/auth"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/config"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
	buildversion "github.com/cf-r2-manager/cf-r2-manager/internal/version"
	"golang.org/x/term"
)

const defaultConfigPath = "/etc/cf-r2-manager/config.yaml"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "server":
		return runServer(args[1:], stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "backup":
		return runBackup(args[1:], stdout, stderr)
	case "restore":
		return runRestore(args[1:], stdout, stderr)
	case "migrate":
		return runMigrate(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "cf-r2-manager %s (commit %s, built %s)\n", buildversion.Version, buildversion.Commit, buildversion.Date)
		return nil
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runInit(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	passwordFile := flags.String("admin-password-file", "", "file containing the administrator password")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	password, err := readAdminPassword(*passwordFile, stderr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.R2.TempDir, 0o750); err != nil {
		return err
	}
	keyPath := secret.ResolveMasterKeyPath(cfg.MasterKeyFile)
	keyCreated := false
	key, err := secret.LoadMasterKey(keyPath)
	if keyPath == cfg.MasterKeyFile && (errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file")) {
		key, err = secret.WriteMasterKey(cfg.MasterKeyFile)
		keyCreated = err == nil
	}
	if err != nil {
		return err
	}
	if _, err := secret.NewCipher(key); err != nil {
		return err
	}
	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		if keyCreated {
			_ = os.Remove(keyPath)
		}
		return err
	}
	defer db.Close()
	if err := auth.NewStore(db).InitializeAdmin(context.Background(), password); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "initialized CF-R2Manager data in %s\n", cfg.DataDir)
	fmt.Fprintf(stdout, "master key: %s\n", keyPath)
	return nil
}

func runServer(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := configureLogger(cfg.LogLevel, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return (app.Server{Config: cfg, Version: buildversion.Version, Logger: logger}).Run(ctx)
}

func runDoctor(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	checks := []struct {
		name string
		run  func() error
	}{
		{"master key", func() error {
			key, err := secret.LoadMasterKey(secret.ResolveMasterKeyPath(cfg.MasterKeyFile))
			if err != nil {
				return err
			}
			_, err = secret.NewCipher(key)
			return err
		}},
		{"database", func() error {
			db, err := database.Open(cfg.DatabasePath)
			if err != nil {
				return err
			}
			defer db.Close()
			initialized, err := auth.NewStore(db).IsInitialized(context.Background())
			if err != nil {
				return err
			}
			if !initialized {
				return errors.New("administrator is not initialized")
			}
			return nil
		}},
		{"temporary directory", func() error {
			if err := os.MkdirAll(cfg.R2.TempDir, 0o750); err != nil {
				return err
			}
			file, err := os.CreateTemp(cfg.R2.TempDir, ".doctor-*")
			if err != nil {
				return err
			}
			name := file.Name()
			_ = file.Close()
			return os.Remove(name)
		}},
	}
	failed := false
	for _, check := range checks {
		if err := check.run(); err != nil {
			failed = true
			fmt.Fprintf(stdout, "FAIL  %-20s %v\n", check.name, err)
		} else {
			fmt.Fprintf(stdout, "OK    %s\n", check.name)
		}
	}
	if failed {
		return errors.New("one or more doctor checks failed")
	}
	return nil
}

func runBackup(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	output := flags.String("output", "", "backup database path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--output is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Backup(context.Background(), db, *output); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "database backed up to %s\n", *output)
	return nil
}

func runRestore(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	input := flags.String("input", "", "backup database path")
	force := flags.Bool("force", false, "replace the current database after preserving it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" {
		return errors.New("--input is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	previous, err := database.Restore(context.Background(), *input, cfg.DatabasePath, *force)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "database restored from %s\n", *input)
	if previous != "" {
		fmt.Fprintf(stdout, "previous database preserved at %s\n", previous)
	}
	return nil
}

func runMigrate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	fmt.Fprintln(stdout, "database migrations are current")
	return nil
}

func readAdminPassword(path string, stderr io.Writer) (string, error) {
	if path != "" {
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}
	if value := os.Getenv("CF_R2_MANAGER_ADMIN_PASSWORD"); value != "" {
		return value, nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(stderr, "Administrator password: ")
		data, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(stderr)
		return string(data), err
	}
	return "", errors.New("set CF_R2_MANAGER_ADMIN_PASSWORD or use --admin-password-file")
}

func configureLogger(levelName string, output io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(levelName) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, "usage: cf-r2-manager <init|server|doctor|backup|restore|migrate|version> [options]")
}
