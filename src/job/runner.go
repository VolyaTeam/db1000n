// MIT License

// Copyright (c) [2022] [Bohdan Ivashko (https://github.com/Arriven)]

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package job

import (
	"bytes"
	"context"
	"flag"
	"math/rand"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Arriven/db1000n/src/job/config"
	"github.com/Arriven/db1000n/src/utils"
	"github.com/Arriven/db1000n/src/utils/metrics"
	"github.com/Arriven/db1000n/src/utils/ota"
	"github.com/Arriven/db1000n/src/utils/templates"
)

// ConfigOptions for fetching job configs for the runner
type ConfigOptions struct {
	PathsCSV       string        // Comma-separated config location URLs
	BackupConfig   string        // Raw backup config
	Format         string        // json or yaml
	RefreshTimeout time.Duration // How often to refresh config
}

var DefaultConfigPathCSV = ""

// NewConfigOptionsWithFlags returns ConfigOptions initialized with command line flags.
func NewConfigOptionsWithFlags() *ConfigOptions {
	var res ConfigOptions

	flag.StringVar(&res.PathsCSV, "c",
		utils.GetEnvStringDefault("CONFIG", DefaultConfigPathCSV),
		"path to config files, separated by a comma, each path can be a web endpoint")
	flag.StringVar(&res.BackupConfig, "b", "", "raw backup config in case the primary one is unavailable")
	flag.StringVar(&res.Format, "format", utils.GetEnvStringDefault("CONFIG_FORMAT", "yaml"), "config format")
	flag.DurationVar(&res.RefreshTimeout, "refresh-interval", utils.GetEnvDurationDefault("REFRESH_INTERVAL", time.Minute),
		"refresh timeout for updating the config")

	return &res
}

// Runner executes jobs according to the (fetched from remote) configuration
type Runner struct {
	cfgOptions    *ConfigOptions
	globalJobsCfg *GlobalConfig
	reporter      metrics.Reporter
}

// NewRunner according to the config
func NewRunner(cfgOptions *ConfigOptions, globalJobsCfg *GlobalConfig, reporter metrics.Reporter) *Runner {
	return &Runner{
		cfgOptions:    cfgOptions,
		globalJobsCfg: globalJobsCfg,
		reporter:      reporter,
	}
}

// Run the runner and block until Stop() is called
func (r *Runner) Run(ctx context.Context, logger *zap.Logger) {
	ctx = context.WithValue(ctx, templates.ContextKey("global"), r.globalJobsCfg)
	lastKnownConfig := &config.RawMultiConfig{}
	refreshTimer := time.NewTicker(r.cfgOptions.RefreshTimeout)

	defer refreshTimer.Stop()
	metrics.IncClient()

	var (
		cancel  context.CancelFunc
		tracker *metrics.StatsTracker
	)

	for {
		rawConfig := config.FetchRawMultiConfig(logger, strings.Split(r.cfgOptions.PathsCSV, ","),
			nonNilConfigOrDefault(lastKnownConfig, &config.RawMultiConfig{
				Body: []byte(nonEmptyStringOrDefault(r.cfgOptions.BackupConfig, config.DefaultConfig)),
			}), r.globalJobsCfg.SkipEncrypted)
		cfg := config.Unmarshal(rawConfig.Body, r.cfgOptions.Format)

		if !bytes.Equal(lastKnownConfig.Body, rawConfig.Body) && cfg != nil { // Only restart jobs if the new config differs from the current one
			logger.Info("new config received, applying")

			lastKnownConfig = rawConfig

			if cancel != nil {
				cancel()
			}

			metric := &metrics.Metrics{} // clear info about previous targets and avoid old jobs from dumping old info to new metrics
			tracker = metrics.NewStatsTracker(metric)

			if rawConfig.Protected {
				logger.Info("config is protected, disabling logs")

				cancel = r.runJobs(ctx, cfg, nil, zap.NewNop())
			} else {
				cancel = r.runJobs(ctx, cfg, metric, logger)
			}
		} else {
			logger.Info("the config has not changed. Keep calm and carry on!")
		}

		// Wait for refresh timer or stop signal
		select {
		case <-refreshTimer.C:
		case <-ctx.Done():
			if cancel != nil {
				cancel()
			}

			return
		}

		reportMetrics(r.reporter, tracker, r.globalJobsCfg.ClientID, logger)
	}
}

func nonEmptyStringOrDefault(s, defaultString string) string {
	if s != "" {
		return s
	}

	return defaultString
}

func nonNilConfigOrDefault(c, defaultConfig *config.RawMultiConfig) *config.RawMultiConfig {
	if c.Body != nil {
		return c
	}

	return defaultConfig
}

func computeCount(count int, scaleFactor float64) int {
	scaledCount := scaleFactor * float64(utils.Max(count, 1))
	if scaledCount > 1 {
		return int(scaledCount)
	}

	// if we have less than 1 goroutine per job we just filter them randomly so that only jobs*scaledCount pass
	if rand.Float64() < scaledCount { //nolint:gosec // Cryptographically secure random not required
		return 1
	}

	return 0
}

func (r *Runner) runJobs(ctx context.Context, cfg *config.MultiConfig, metric *metrics.Metrics, logger *zap.Logger) (cancel context.CancelFunc) {
	ctx, cancel = context.WithCancel(ctx)
	ctx = context.WithValue(ctx, templates.ContextKey("goos"), runtime.GOOS)
	ctx = context.WithValue(ctx, templates.ContextKey("goarch"), runtime.GOARCH)
	ctx = context.WithValue(ctx, templates.ContextKey("version"), ota.Version)

	var jobInstancesCount int

	for i := range cfg.Jobs {
		if len(cfg.Jobs[i].Filter) != 0 && strings.TrimSpace(templates.ParseAndExecute(logger, cfg.Jobs[i].Filter, ctx)) != "true" {
			logger.Info("There is a filter defined for a job but this client doesn't pass it - skip the job")

			continue
		}

		job := Get(cfg.Jobs[i].Type)
		if job == nil {
			logger.Warn("unknown job", zap.String("type", cfg.Jobs[i].Type))

			continue
		}

		if r.globalJobsCfg.ScaleFactor > 0 {
			cfg.Jobs[i].Count = computeCount(cfg.Jobs[i].Count, r.globalJobsCfg.ScaleFactor)
		}

		cfgMap := make(map[string]any)
		if err := utils.Decode(cfg.Jobs[i], &cfgMap); err != nil {
			logger.Fatal("failed to encode cfg map")
		}

		ctx := context.WithValue(ctx, templates.ContextKey("config"), cfgMap)
		ctx = context.WithValue(ctx, templates.ContextKey("metrics"), metric)

		for j := 0; j < cfg.Jobs[i].Count; j++ {
			if cfg.Jobs[i].Name != "" {
				logger.Info("Attacking", zap.String("target", cfg.Jobs[i].Name))
			}

			go func(i int) {
				defer utils.PanicHandler(logger)

				if _, err := job(ctx, cfg.Jobs[i].Args, r.globalJobsCfg, metric.NewAccumulator(uuid.NewString()), logger); err != nil {
					logger.Error("error running job",
						zap.String("name", cfg.Jobs[i].Name),
						zap.String("type", cfg.Jobs[i].Type),
						zap.Error(err))
				}
			}(i)

			jobInstancesCount++
		}
	}

	logger.Info("job instances (re)started", zap.Int("count", jobInstancesCount))

	return cancel
}

func reportMetrics(reporter metrics.Reporter, tracker *metrics.StatsTracker, clientID string, logger *zap.Logger) {
	if reporter != nil && tracker != nil {
		reporter.WriteSummary(tracker)

		// TODO: get rid of this
		if err := metrics.ReportStatistics(0, clientID); err != nil {
			logger.Debug("error reporting statistics", zap.Error(err))
		}
	}
}
