// Package config reads every setting the server needs and validates them before anything else runs.
//
// Everything fails at startup or not at all. A self-hosted server (H1) that accepts a missing or
// malformed setting and only reports it on the first real request costs its operator a debugging
// session for what should have been one line of output at boot.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// SignupMode decides who may create an account over HTTP.
type SignupMode string

const (
	// SignupModeClosed refuses account creation over HTTP entirely. The default, because a
	// self-hosted server reachable from the internet with open registration is a service somebody
	// else will use. Accounts are created with the `create-account` subcommand instead.
	SignupModeClosed SignupMode = "closed"

	// SignupModeOpen lets anyone register. Appropriate for a private network, or a deliberate
	// decision paired with the rate limiting of S6.
	SignupModeOpen SignupMode = "open"
)

// Config is the fully validated configuration. Every field is safe to use without a further check.
type Config struct {
	AdminListenAddress string
	SyncListenAddress  string
	DatabaseURL        string
	LogLevel           slog.Level
	SignupMode         SignupMode

	// How long in-flight requests get to finish after a shutdown signal arrives. Sync pushes are
	// short, so this is generous rather than tuned.
	ShutdownTimeout time.Duration

	// How long a push response stays replayable for the device that sent it.
	//
	// It only has to outlive a client's retry of a batch whose response was lost, which is minutes
	// at the outside; a day of margin costs one small row per push and makes the window a
	// non-question for anyone self-hosting.
	BatchReplayWindow time.Duration
}

const (
	adminListenAddressSetting = "VIVE_ADMIN_LISTEN_ADDR"
	syncListenAddressSetting  = "VIVE_SYNC_LISTEN_ADDR"
	databaseURLSetting        = "VIVE_DATABASE_URL"
	logLevelSetting           = "VIVE_LOG_LEVEL"
	shutdownTimeoutSetting    = "VIVE_SHUTDOWN_TIMEOUT"
	signupModeSetting         = "VIVE_SIGNUP_MODE"

	// syncPlan.md §7 named this VIVE_BATCH_REPLAY_HOURS. It takes a duration instead, because every
	// other time setting here does: a surface where one knob is a bare number of hours is one where
	// an operator eventually writes `24h` into it and gets a parse error, or writes `48` into one of
	// the others and gets 48 nanoseconds.
	batchReplayWindowSetting = "VIVE_BATCH_REPLAY_WINDOW"
)

// Load reads configuration from the environment, having first merged any `.env` file beside the
// binary.
//
// A real environment variable always wins over the `.env` file — godotenv.Load leaves existing
// variables alone, which is what makes the file a convenience for development rather than a thing
// that can silently override what an operator set in a systemd unit or a compose file.
func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("reading .env: %w", err)
	}

	logLevel, err := readLogLevelSetting(logLevelSetting, slog.LevelInfo)
	if err != nil {
		return Config{}, err
	}

	shutdownTimeout, err := readDurationSetting(shutdownTimeoutSetting, 15*time.Second)
	if err != nil {
		return Config{}, err
	}

	signupMode, err := readSignupModeSetting(signupModeSetting, SignupModeClosed)
	if err != nil {
		return Config{}, err
	}

	batchReplayWindow, err := readDurationSetting(batchReplayWindowSetting, 24*time.Hour)
	if err != nil {
		return Config{}, err
	}

	settings := Config{
		AdminListenAddress: readStringSetting(adminListenAddressSetting, ":8080"),
		SyncListenAddress:  readStringSetting(syncListenAddressSetting, ":8281"),
		DatabaseURL:        readStringSetting(databaseURLSetting, ""),
		LogLevel:           logLevel,
		SignupMode:         signupMode,
		ShutdownTimeout:    shutdownTimeout,
		BatchReplayWindow:  batchReplayWindow,
	}

	if settings.AdminListenAddress == settings.SyncListenAddress {
		return Config{}, fmt.Errorf("%s and %s must use different addresses", adminListenAddressSetting, syncListenAddressSetting)
	}

	// The database URL carries a password, so it is the one setting that must never reach a log
	// line or an error message. Report only that it is absent.
	if settings.DatabaseURL == "" {
		return Config{}, fmt.Errorf("%s is required", databaseURLSetting)
	}

	return settings, nil
}

func readStringSetting(settingName string, fallbackValue string) string {
	value := strings.TrimSpace(os.Getenv(settingName))
	if value == "" {
		return fallbackValue
	}
	return value
}

func readDurationSetting(settingName string, fallbackValue time.Duration) (time.Duration, error) {
	raw := readStringSetting(settingName, "")
	if raw == "" {
		return fallbackValue, nil
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 15s: %w", settingName, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", settingName, raw)
	}
	return parsed, nil
}

// readSignupModeSetting refuses an unrecognised value rather than falling back to the default.
//
// A typo in this particular setting is the difference between a closed server and an open one, and
// silently defaulting would mean `VIVE_SIGNUP_MODE=opne` reads as closed while its author believes
// it reads as open. Whichever way that mistake resolves, it should be loud.
func readSignupModeSetting(settingName string, fallbackValue SignupMode) (SignupMode, error) {
	raw := readStringSetting(settingName, "")
	if raw == "" {
		return fallbackValue, nil
	}

	switch mode := SignupMode(strings.ToLower(raw)); mode {
	case SignupModeClosed, SignupModeOpen:
		return mode, nil
	default:
		return "", fmt.Errorf("%s must be one of closed, open; got %q", settingName, raw)
	}
}

func readLogLevelSetting(settingName string, fallbackValue slog.Level) (slog.Level, error) {
	raw := readStringSetting(settingName, "")
	if raw == "" {
		return fallbackValue, nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return 0, fmt.Errorf("%s must be one of debug, info, warn, error: %w", settingName, err)
	}
	return level, nil
}
