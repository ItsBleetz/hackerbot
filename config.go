package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HackerOneBaseURL        string `json:"hackerone_base_url"`
	HackerOneUsername       string `json:"hackerone_username,omitempty"`
	HackerOneAPIToken       string `json:"hackerone_api_token,omitempty"`
	ProgramWebhookURL       string `json:"program_webhook_url,omitempty"`
	ReportWebhookURL        string `json:"report_webhook_url,omitempty"`
	StateFile               string `json:"state_file"`
	ProgramPollInterval     string `json:"program_poll_interval"`
	ReportPollInterval      string `json:"report_poll_interval"`
	RequestInterval         string `json:"request_interval"`
	ReportRequestInterval   string `json:"report_request_interval"`
	ScopeRequestInterval    string `json:"scope_request_interval"`
	RequestTimeout          string `json:"request_timeout"`
	ReportsEnabled          bool   `json:"reports_enabled"`
	ReportNotificationMode  string `json:"report_notification_mode"`
	ReportNotifyOwnComments bool   `json:"report_notify_own_comments"`
	ReportThreadsEnabled    bool   `json:"report_threads_enabled"`

	programPoll  time.Duration
	reportPoll   time.Duration
	requestDelay time.Duration
	reportDelay  time.Duration
	scopeDelay   time.Duration
	timeout      time.Duration
}

func defaultConfig() Config {
	return Config{
		HackerOneBaseURL:       "https://api.hackerone.com",
		StateFile:              "hackerbot.db",
		ProgramPollInterval:    "30m",
		ReportPollInterval:     "5m",
		RequestInterval:        "150ms",
		ReportRequestInterval:  "210ms",
		ScopeRequestInterval:   "1250ms",
		RequestTimeout:         "30s",
		ReportsEnabled:         true,
		ReportNotificationMode: "summary",
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}

	overrideEnv(&cfg.HackerOneUsername, "HACKERONE_USERNAME")
	overrideEnv(&cfg.HackerOneAPIToken, "HACKERONE_API_TOKEN")
	overrideEnv(&cfg.ProgramWebhookURL, "DISCORD_PROGRAM_WEBHOOK")
	overrideEnv(&cfg.ReportWebhookURL, "DISCORD_REPORT_WEBHOOK")
	overrideEnv(&cfg.StateFile, "HACKERBOT_STATE_FILE")
	overrideEnv(&cfg.ReportNotificationMode, "HACKERBOT_REPORT_NOTIFICATION_MODE")
	if err := overrideEnvBool(&cfg.ReportsEnabled, "HACKERBOT_REPORTS_ENABLED"); err != nil {
		return Config{}, err
	}
	if err := overrideEnvBool(&cfg.ReportNotifyOwnComments, "HACKERBOT_REPORT_NOTIFY_OWN_COMMENTS"); err != nil {
		return Config{}, err
	}
	if err := overrideEnvBool(&cfg.ReportThreadsEnabled, "HACKERBOT_REPORT_THREADS_ENABLED"); err != nil {
		return Config{}, err
	}

	if strings.TrimSpace(cfg.HackerOneBaseURL) == "" {
		return Config{}, errors.New("hackerone_base_url must not be empty")
	}
	cfg.HackerOneBaseURL = strings.TrimRight(cfg.HackerOneBaseURL, "/")
	apiURL, urlErr := url.Parse(cfg.HackerOneBaseURL)
	if urlErr != nil || apiURL.Scheme != "https" || !strings.EqualFold(apiURL.Host, "api.hackerone.com") || apiURL.Path != "" || apiURL.User != nil || apiURL.RawQuery != "" || apiURL.Fragment != "" {
		return Config{}, errors.New("hackerone_base_url must be exactly https://api.hackerone.com")
	}
	if cfg.HackerOneUsername == "" || cfg.HackerOneAPIToken == "" {
		return Config{}, errors.New("set HACKERONE_USERNAME and HACKERONE_API_TOKEN")
	}
	if cfg.StateFile == "" {
		return Config{}, errors.New("state_file must not be empty")
	}
	cfg.ReportNotificationMode = strings.ToLower(strings.TrimSpace(cfg.ReportNotificationMode))
	if cfg.ReportNotificationMode != "summary" && cfg.ReportNotificationMode != "detailed" {
		return Config{}, errors.New("report_notification_mode must be summary or detailed")
	}
	if !filepath.IsAbs(cfg.StateFile) {
		cfg.StateFile = filepath.Join(filepath.Dir(path), cfg.StateFile)
	}

	var parseErr error
	if cfg.programPoll, parseErr = positiveDuration("program_poll_interval", cfg.ProgramPollInterval); parseErr != nil {
		return Config{}, parseErr
	}
	if cfg.reportPoll, parseErr = positiveDuration("report_poll_interval", cfg.ReportPollInterval); parseErr != nil {
		return Config{}, parseErr
	}
	if cfg.requestDelay, parseErr = positiveDuration("request_interval", cfg.RequestInterval); parseErr != nil {
		return Config{}, parseErr
	}
	if cfg.requestDelay < 110*time.Millisecond {
		return Config{}, errors.New("request_interval must be at least 110ms to stay safely below HackerOne's 600 reads/minute limit")
	}
	if cfg.reportDelay, parseErr = positiveDuration("report_request_interval", cfg.ReportRequestInterval); parseErr != nil {
		return Config{}, parseErr
	}
	if cfg.reportDelay < 210*time.Millisecond {
		return Config{}, errors.New("report_request_interval must be at least 210ms to stay safely below HackerOne's 300 report reads/minute limit")
	}
	if cfg.scopeDelay, parseErr = positiveDuration("scope_request_interval", cfg.ScopeRequestInterval); parseErr != nil {
		return Config{}, parseErr
	}
	if cfg.scopeDelay < 1250*time.Millisecond {
		return Config{}, errors.New("scope_request_interval must be at least 1250ms to stay safely below HackerOne's 50 structured-scope requests/minute limit")
	}
	if cfg.timeout, parseErr = positiveDuration("request_timeout", cfg.RequestTimeout); parseErr != nil {
		return Config{}, parseErr
	}
	return cfg, nil
}

func overrideEnv(dst *string, name string) {
	if value, ok := os.LookupEnv(name); ok {
		*dst = value
	}
}

func overrideEnvBool(dst *bool, name string) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("%s must be true or false: %q", name, value)
	}
	*dst = parsed
	return nil
}

func positiveDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration: %q", name, value)
	}
	return d, nil
}
