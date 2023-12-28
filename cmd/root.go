/*
Copyright © 2023 pyama86 <www.kazu.com@gmail.com>
*/
package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/mitchellh/go-homedir"
	"github.com/pyama86/git-assets-canly-releaser/lib"
	redis "github.com/redis/go-redis/v9"
	"github.com/thoas/go-funk"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "git-assets-canly-releaser",
	Short: "This command downloads release assets from GitHub and deploys them.",
	Long:  ``,
	Run: func(cmd *cobra.Command, args []string) {
		config, err := loadConfig()
		if err != nil {
			slog.Error(fmt.Sprintf("failed to load config: %s", err))
			os.Exit(1)
		}

		logger, err := getLogger(config, config.LogLevel)
		if err != nil {
			slog.Error(fmt.Sprintf("failed to init logger: %s", err))
			os.Exit(1)
		}
		slog.SetDefault(logger)

		if err := runServer(config); err != nil {
			slog.Error(fmt.Sprintf("failed to run server: %s", err))
			os.Exit(1)
		}
	},
}

func deploy(cmd, tag, file string) error {
	out, err := executeCommand(cmd, tag, file)
	if err != nil {
		return fmt.Errorf("failed to execute command: %s, %s", err, out)
	}
	return nil
}

func getCurrentStates(state *lib.State) (string, *lib.LocalState, []string, error) {
	stableRelease, err := state.GetStableReleaseTag()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			slog.Info("stable release tag notfound")
		} else {
			return "", nil, nil, err
		}
	}

	avoidRelease, err := state.GetAvoidReleaseTag()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			slog.Info("avoid release tag notfound")
		} else {
			return "", nil, nil, err
		}
	}

	localState, err := state.GetLocalState()
	if err != nil {
		return "", nil, nil, err
	}
	return stableRelease, localState, avoidRelease, nil
}

func handleRollout(config *lib.Config, github *lib.GitHub, state *lib.State) error {
	stableRelease, localState, avoidRelease, err := getCurrentStates(state)
	if err != nil {
		return err
	}
	if stableRelease != "" &&
		localState.LastInstalledTag != stableRelease &&
		!funk.Contains(avoidRelease, stableRelease) {

		got, err := state.TryRolloutLock(stableRelease)
		if err != nil {
			return err
		}

		if got {
			_, downloadFile, err := github.DownloadReleaseAsset(stableRelease)
			if err != nil {
				return fmt.Errorf("can't get release asset:%s %s", stableRelease, err)
			}

			if err := deploy(config.DeployCommand, stableRelease, downloadFile); err != nil {
				return fmt.Errorf("deploy command failed: %s", err)
			}
			localState.LastInstalledTag = stableRelease

		}
		if err := state.SaveLocalState(localState); err != nil {
			return err
		}
	}
	return nil

}

func handleCanaryRelease(config *lib.Config, github *lib.GitHub, state *lib.State) error {
	stableRelease, localState, avoidRelease, err := getCurrentStates(state)
	if err != nil {
		return err
	}

	latestTag, downloadFile, err := github.DownloadReleaseAsset(lib.LatestTag)
	if err != nil {
		if errors.Is(err, lib.AssetsNotFound) {
			slog.Info("latest release notfound")
			if viper.GetBool("once") {
				return nil
			}
			return nil
		} else {
			return err
		}
	}

	if localState.LastInstalledTag != latestTag && !funk.Contains(avoidRelease, latestTag) {
		got, err := state.TryCanaryReleaseLock(latestTag)
		if err != nil {
			return err
		}
		if got {
			slog.Info("canary release locked", "tag", latestTag)
			if err := deploy(config.DeployCommand, latestTag, downloadFile); err != nil {
				return fmt.Errorf("deploy command failed: %s", err)
			}

			slog.Info("deploy command success", "tag", latestTag)
			if err := runHealthCheck(config, latestTag, downloadFile); err != nil {
				slog.Error("health check command failed", err)
				if err := state.SaveAvoidReleaseTag(latestTag); err != nil {
					return fmt.Errorf("can't save avoid tag:%s", err)
				}

				// try rollback
				rollbackTag := stableRelease
				if rollbackTag == "" {
					rollbackTag = localState.LastInstalledTag
				}
				if rollbackTag == "" {
					return fmt.Errorf("can't decided rollback tag")
				}

				return handleRollback(config, github, rollbackTag)

			} else {
				if err := state.SaveStableReleaseTag(latestTag); err != nil {
					return fmt.Errorf("can't save stable tag:%s", err)
				}

				if err := state.UnlockCanaryRelease(); err != nil {
					return fmt.Errorf("can't unlock canary release tag")
				}
				localState.LastInstalledTag = latestTag
			}

			if err := state.SaveLocalState(localState); err != nil {
				return err
			}

		}
	}
	return nil
}

func handleRollback(config *lib.Config, github *lib.GitHub, rollbackTag string) error {
	_, downloadFile, err := github.DownloadReleaseAsset(rollbackTag)
	if errors.Is(err, lib.AssetsNotFound) {
		return fmt.Errorf("can't get release asset:%s", rollbackTag)
	}

	if err := deploy(config.RollbackCommand, rollbackTag, downloadFile); err != nil {
		return fmt.Errorf("rollback error: %s", err)
	}
	return nil

}
func runServer(config *lib.Config) error {
	github, err := lib.NewGitHub(config)
	if err != nil {
		return err
	}

	state, err := lib.NewState(config)
	if err != nil {
		return err
	}

	gitTicker := time.NewTicker(config.RepositryPollingInterval)
	if viper.GetBool("once") {
		gitTicker = time.NewTicker(time.Nanosecond)
	}
	defer gitTicker.Stop()

	rolloutTicker := time.NewTicker(config.RolloutWindow)
	if viper.GetBool("once") {
		rolloutTicker = time.NewTicker(time.Nanosecond)
	}
	defer rolloutTicker.Stop()

	for {
		select {
		case <-rolloutTicker.C:
			if err := handleRollout(config, github, state); err != nil {
				return err
			}
			if viper.GetBool("once") {
				rolloutTicker.Stop()
			}
		case <-gitTicker.C:
			if err := handleCanaryRelease(config, github, state); err != nil {
				return err
			}
			if viper.GetBool("once") {
				return nil
			}
		}
	}
}

func runHealthCheck(config *lib.Config, tag, file string) error {
	healthCheckTick := time.NewTicker(config.HealthCheckInterval)
	defer healthCheckTick.Stop()
	canaryReleaseTick := time.NewTicker(config.CanaryRolloutWindow)
	defer canaryReleaseTick.Stop()

	f := func() error {
		out, err := executeCommand(config.HealthCheckCommand, tag, file)
		if err != nil {
			return fmt.Errorf("health check command failed: %s, %s", err, out)
		}
		return nil
	}
	if err := f(); err != nil {
		return err
	}

	for {
		select {
		case <-healthCheckTick.C:
			if err := f(); err != nil {
				return err
			}

		case <-canaryReleaseTick.C:
			return nil
		}
	}
}

func executeCommand(command string, tag, file string) ([]byte, error) {
	cmd := exec.Command(command)
	cmd.Env = append(os.Environ(), fmt.Sprintf("RELEASE_TAG=%s", tag))
	cmd.Env = append(cmd.Env, fmt.Sprintf("ASSET_FILE=%s", file))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	slog.Debug("command result", "command", command, "out", string(out))
	return out, nil
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func getLogger(config *lib.Config, level string) (*slog.Logger, error) {
	logLevel := slog.LevelInfo
	var logOutput io.Writer
	switch level {
	case "info":
		logLevel = slog.LevelInfo
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level: %s", level)
	}
	ops := slog.HandlerOptions{
		Level: logLevel,
	}
	logOutput = os.Stdout

	return slog.New(slog.NewJSONHandler(logOutput, &ops)), nil
}

func loadConfig() (*lib.Config, error) {
	p, err := homedir.Expand(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get config path: %s", err)
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return nil, fmt.Errorf("failed to get config path: %s", err)
	}
	viper.SetConfigType("toml")
	viper.SetEnvPrefix("GACR")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if _, err := os.Stat(p); err == nil {
		c, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}

		if err != nil {
			return nil, fmt.Errorf("failed to read config: %s", err)
		}

		if err := viper.ReadConfig(bytes.NewReader(c)); err != nil {
			return nil, fmt.Errorf("failed to read config: %s", err)
		}
	} else {
		slog.Warn("config file not found", slog.String("path", p))
	}

	config := lib.Config{}
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %s", err)
	}

	validate := validator.New(validator.WithRequiredStructEnabled())
	err = validate.Struct(&config)
	if err != nil {
		return nil, fmt.Errorf("faileh to validate config: %s", err)
	}
	return &config, nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "~/gacr.conf", "config file (default is $HOME/.git-assets-canly-releaser.toml)")
	rootCmd.PersistentFlags().String("repo", "", "GitHub repository name")
	viper.BindPFlag("repo", rootCmd.PersistentFlags().Lookup("repo"))

	rootCmd.PersistentFlags().String("github-token", "", "GitHub token")
	viper.BindPFlag("github_token", rootCmd.PersistentFlags().Lookup("github-token"))

	rootCmd.PersistentFlags().String("github-api", "https://api.github.com", "GitHub API endpoint")
	viper.BindPFlag("github_api", rootCmd.PersistentFlags().Lookup("github-api"))

	rootCmd.PersistentFlags().String("deploy-command", "", "Deploy command")
	viper.BindPFlag("deploy_command", rootCmd.PersistentFlags().Lookup("deploy-command"))

	rootCmd.PersistentFlags().String("rollback-command", "", "Rollback command")
	viper.BindPFlag("rollback_command", rootCmd.PersistentFlags().Lookup("rollback-command"))

	rootCmd.PersistentFlags().String("healthcheck-command", "", "HealthCheck command")
	viper.BindPFlag("healthcheck_command", rootCmd.PersistentFlags().Lookup("healthcheck-command"))

	rootCmd.PersistentFlags().String("slack-webhook-url", "", "Slack webhook URL")
	viper.BindPFlag("slack_webhook_url", rootCmd.PersistentFlags().Lookup("slack-webhook-url"))

	rootCmd.PersistentFlags().String("redis-host", "127.0.0.1", "Redis host")
	viper.BindPFlag("redis.host", rootCmd.PersistentFlags().Lookup("redis-host"))

	rootCmd.PersistentFlags().Int("redis-port", 6379, "Redis port")
	viper.BindPFlag("redis.port", rootCmd.PersistentFlags().Lookup("redis-port"))

	rootCmd.PersistentFlags().String("redis-password", "", "Redis password")
	viper.BindPFlag("redis.password", rootCmd.PersistentFlags().Lookup("redis-password"))

	rootCmd.PersistentFlags().Int("redis-db", 1, "Redis DB")
	viper.BindPFlag("redis.db", rootCmd.PersistentFlags().Lookup("redis-db"))

	rootCmd.PersistentFlags().String("package-name-pattern", "", "Package name pattern")
	viper.BindPFlag("package_name_pattern", rootCmd.PersistentFlags().Lookup("package-name-pattern"))

	rootCmd.PersistentFlags().String("log-level", "info", "Log level")
	viper.BindPFlag("log_level", rootCmd.PersistentFlags().Lookup("log-level"))

	rootCmd.PersistentFlags().String("save-assets-path", "/usr/local/src", "assets download path")
	viper.BindPFlag("save_assets_path", rootCmd.PersistentFlags().Lookup("save-assets-path"))

	rootCmd.PersistentFlags().Duration("canary-rollout-window", 15*time.Minute, "canary release rollout window")
	viper.BindPFlag("canary_rollout_window", rootCmd.PersistentFlags().Lookup("canary-rollout-window"))

	rootCmd.PersistentFlags().Duration("rollout-window", 1*time.Minute, "release rollout window")
	viper.BindPFlag("rollout_window", rootCmd.PersistentFlags().Lookup("rollout-window"))

	rootCmd.PersistentFlags().Duration("health-check-interval", 1*time.Minute, "health check interval")
	viper.BindPFlag("healthcheck_interval", rootCmd.PersistentFlags().Lookup("health-check-interval"))

	rootCmd.PersistentFlags().Duration("repository-polling-interval", 1*time.Minute, "repository polling interval")
	viper.BindPFlag("repository_polling_interval", rootCmd.PersistentFlags().Lookup("repository-polling-interval"))

	rootCmd.PersistentFlags().Bool("once", false, "one shot mode")
	viper.BindPFlag("once", rootCmd.PersistentFlags().Lookup("once"))

}
