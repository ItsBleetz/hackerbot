package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const version = "1.8.0"

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	envPath := flag.String("env", "", "path to an environment file (default: .env next to config)")
	handle := flag.String("handle", "", "fetch one program and send it immediately")
	dummy := flag.Bool("dummy", false, "fetch three programs and two reports from HackerOne and preview them in Discord without changing SQLite")
	once := flag.Bool("once", false, "run enabled monitors once and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("hackerbot " + version)
		return 0
	}
	resolvedEnvPath := *envPath
	envRequired := strings.TrimSpace(resolvedEnvPath) != ""
	if !envRequired {
		resolvedEnvPath = filepath.Join(filepath.Dir(*configPath), ".env")
	}
	if err := loadDotEnv(resolvedEnvPath, envRequired); err != nil {
		log.Printf("configuration error: %v", err)
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Printf("configuration error: %v", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *dummy {
		if strings.TrimSpace(cfg.ProgramWebhookURL) == "" {
			log.Printf("configuration error: set DISCORD_PROGRAM_WEBHOOK for -dummy")
			return 2
		}
		if strings.TrimSpace(cfg.ReportWebhookURL) == "" {
			log.Printf("configuration error: set DISCORD_REPORT_WEBHOOK for -dummy")
			return 2
		}
		if err := sendDummyNotifications(ctx, cfg); err != nil {
			log.Printf("dummy Discord test failed: %v", err)
			return 1
		}
		log.Printf("dummy Discord test completed; HackerOne was read and the SQLite baseline was not accessed")
		return 0
	}
	store, err := openStateStore(cfg.StateFile)
	if err != nil {
		log.Printf("state error: %v", err)
		return 2
	}
	defer store.Close()
	monitor := newMonitor(cfg, store)

	if strings.TrimSpace(*handle) != "" {
		if strings.TrimSpace(cfg.ProgramWebhookURL) == "" {
			log.Printf("configuration error: set DISCORD_PROGRAM_WEBHOOK for -handle")
			return 2
		}
		if err := monitor.SendHandle(ctx, strings.TrimSpace(*handle)); err != nil {
			log.Printf("send program: %v", err)
			return 1
		}
		log.Printf("sent program %s to Discord", strings.TrimSpace(*handle))
		return 0
	}

	programsActive := cfg.ProgramsEnabled && strings.TrimSpace(cfg.ProgramWebhookURL) != ""
	if !cfg.ProgramsEnabled {
		log.Printf("private-program monitoring disabled by programs_enabled=false")
	}
	if cfg.ProgramsEnabled && !programsActive {
		log.Printf("configuration error: set DISCORD_PROGRAM_WEBHOOK or set programs_enabled=false")
		return 2
	}
	reportsActive := cfg.ReportsEnabled && strings.TrimSpace(cfg.ReportWebhookURL) != ""
	if !cfg.ReportsEnabled {
		log.Printf("report monitoring disabled by reports_enabled=false")
	}
	if cfg.ReportsEnabled && !reportsActive {
		log.Printf("report monitoring disabled until DISCORD_REPORT_WEBHOOK is configured")
	}
	if *once {
		failed := false
		if programsActive {
			if err := monitor.CheckPrograms(ctx); err != nil {
				log.Printf("private-program check failed: %v", err)
				failed = true
			}
		}
		if reportsActive {
			if err := monitor.CheckReports(ctx); err != nil {
				log.Printf("report check failed: %v", err)
				failed = true
			}
		}
		if failed {
			return 1
		}
		return 0
	}

	var wg sync.WaitGroup
	if programsActive {
		log.Printf("starting private-program monitor every %s", cfg.programPoll)
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSchedule(ctx, cfg.programPoll, "private-program", monitor.CheckPrograms)
		}()
	}
	if reportsActive {
		log.Printf("starting report monitor every %s", cfg.reportPoll)
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSchedule(ctx, cfg.reportPoll, "report", monitor.CheckReports)
		}()
	}
	if !programsActive && !reportsActive {
		log.Printf("no monitors enabled; nothing to run")
		return 0
	}
	wg.Wait()
	log.Printf("hackerbot stopped")
	return 0
}

func runSchedule(ctx context.Context, interval time.Duration, name string, check func(context.Context) error) {
	if err := check(ctx); err != nil && ctx.Err() == nil {
		log.Printf("%s check failed: %v", name, err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := check(ctx); err != nil && ctx.Err() == nil {
				log.Printf("%s check failed: %v", name, err)
			}
		}
	}
}
