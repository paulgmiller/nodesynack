package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

func writeMetricsToFile(path string) error {
	// Gather current metrics
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return err
	}

	// Write atomically: temp file then rename
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, "metrics-*.prom")
	if err != nil {
		return err
	}
	defer tmpFile.Close()

	enc := expfmt.NewEncoder(tmpFile, expfmt.FmtText)

	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}

	if err := tmpFile.Sync(); err != nil {
		return err
	}

	return os.Rename(tmpFile.Name(), path)
}

func flusthCounters(ctx context.Context, path string) {
	// periodically flush metrics to file, e.g. every 15s
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeMetricsToFile("/metrics/nodesynack.prom"); err != nil {
				slog.ErrorContext(ctx, "failed to write metrics", "error", err)
			}
		}
	}
}
