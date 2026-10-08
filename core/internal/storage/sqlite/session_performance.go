package sqlite

import (
	"sort"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

type sessionPerformance struct {
	ttftSum, ttftCount       int64
	outputTokens, durationMs int64
	calls                    map[string]*sessionCallInterval
}

type sessionCallInterval struct {
	id    string
	turn  int
	start time.Time
	end   *time.Time
}

func (stats *sessionPerformance) observe(rootID string, turn int, record contract.RequestRecord) {
	if record.InputProtocol == contract.ProtocolOpenAIModels || record.InputProtocol == contract.ProtocolGoogleModels {
		return
	}
	if stats.calls == nil {
		stats.calls = make(map[string]*sessionCallInterval)
	}
	call := stats.calls[rootID]
	if call == nil {
		call = &sessionCallInterval{id: rootID, turn: turn, start: record.StartedAt}
		stats.calls[rootID] = call
	}
	if record.StartedAt.Before(call.start) {
		call.start = record.StartedAt
	}
	// Children establish when a retried call really began. Only its final root
	// establishes completion, so retry backoff never becomes tool execution.
	if string(record.ID) == rootID && (record.Error == nil || record.Error.Code != "core_interrupted") {
		call.end = record.CompletedAt
	}
	if record.Streaming && record.FirstTokenMs != nil {
		stats.ttftSum += int64(*record.FirstTokenMs)
		stats.ttftCount++
	}
	// TPS divides by the whole call duration, TTFT included: non-streaming calls
	// have no first token, and streamed output can arrive in one burst after it.
	if record.LatencyMs == nil || *record.LatencyMs <= 0 || record.Usage == nil ||
		record.Usage.BillingIncomplete || record.Usage.OutputTokens <= 0 {
		return
	}
	stats.outputTokens += int64(record.Usage.OutputTokens)
	stats.durationMs += int64(*record.LatencyMs)
}

func (stats *sessionPerformance) apply(session *contract.RequestSession) {
	if stats.ttftCount > 0 {
		average := float64(stats.ttftSum) / float64(stats.ttftCount)
		session.AverageTTFTMs = &average
	}
	if stats.durationMs > 0 {
		rate := float64(stats.outputTokens) * 1000 / float64(stats.durationMs)
		session.OutputTokensPerSecond = &rate
	}
	if len(stats.calls) == 0 {
		return
	}
	calls := make([]*sessionCallInterval, 0, len(stats.calls))
	for _, call := range stats.calls {
		// Historical calls without turn identity cannot separate tools from user idle time.
		if call.turn == 0 {
			return
		}
		calls = append(calls, call)
	}
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].start.Equal(calls[j].start) {
			return calls[i].id < calls[j].id
		}
		return calls[i].start.Before(calls[j].start)
	})
	var total int64
	var end *time.Time
	for i, call := range calls {
		if i == 0 || call.turn != calls[i-1].turn {
			end = call.end
			continue
		}
		if end == nil {
			return
		} // Interrupted or still-running predecessor: gap unknown.
		total += max(0, call.start.Sub(*end).Milliseconds())
		if call.end == nil || call.end.After(*end) {
			end = call.end
		}
	}
	session.ToolDurationMs = &total
}
