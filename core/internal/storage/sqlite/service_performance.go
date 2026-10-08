package sqlite

import (
	"database/sql"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

type servicePerformance struct {
	cacheInput, cacheRead, output, durationMs int64
	cacheSamples, speedSamples                int64
}

func (stats *servicePerformance) observe(usage contract.Usage, latency sql.NullInt64) {
	if usage.BillingIncomplete {
		return
	}
	if usage.InputTokens > 0 && usage.CacheReadTokens != nil && *usage.CacheReadTokens <= usage.InputTokens {
		stats.cacheInput += int64(usage.InputTokens)
		stats.cacheRead += int64(*usage.CacheReadTokens)
		stats.cacheSamples++
	}
	// Whole call duration, TTFT included, as sessionPerformance measures it.
	if latency.Valid && latency.Int64 > 0 && usage.OutputTokens > 0 {
		stats.output += int64(usage.OutputTokens)
		stats.durationMs += latency.Int64
		stats.speedSamples++
	}
}

func (stats *servicePerformance) summary() *storage.ServicePerformance {
	result := &storage.ServicePerformance{}
	if stats == nil {
		return result
	}
	result.CacheSamples, result.SpeedSamples = stats.cacheSamples, stats.speedSamples
	if stats.cacheInput > 0 {
		rate := float64(stats.cacheRead) / float64(stats.cacheInput)
		result.CacheHitRate = &rate
	}
	if stats.durationMs > 0 {
		rate := float64(stats.output) * 1000 / float64(stats.durationMs)
		result.OutputTokensPerSecond = &rate
	}
	return result
}
