// Package metrics exposes Prometheus instrumentation (C.2): Go runtime + process
// collectors, HTTP request count/latency, and domain counters for runs, log
// ingest, schedule publishes and webhook syncs.
//
// A package-level default registry (`std`) lets subsystems record via one-line
// helpers (e.g. metrics.RunFinished("failure")) without threading a struct
// through every constructor. The api layer serves the same registry at /metrics
// and feeds it HTTP observations. Tests can build an isolated Registry with New.
package metrics

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// runnerCount holds the current active-runner counting function. It's a global
// (swappable) holder so the active_runners gauge — baked into every Registry —
// can be fed a DB-backed closure by the api layer without the metrics package
// importing the DB. nil holder ⇒ the gauge reports 0.
var runnerCount atomic.Value // func() float64

// SetRunnerCountFunc installs the closure the active_runners gauge calls on each
// scrape. Safe to call once at startup (the api layer wires a DB COUNT).
func SetRunnerCountFunc(fn func() float64) { runnerCount.Store(fn) }

func activeRunners() float64 {
	if v, ok := runnerCount.Load().(func() float64); ok && v != nil {
		return v()
	}
	return 0
}

// Registry bundles a Prometheus registry with the metric vectors Cronomicon emits.
type Registry struct {
	reg *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec

	runsStarted     prometheus.Counter
	runsFinished    *prometheus.CounterVec
	logIngestBytes  prometheus.Counter
	logIngestChunks prometheus.Counter
	schedulePublish prometheus.Counter
	webhookSyncs    *prometheus.CounterVec

	backupLastSuccess    prometheus.Gauge
	backupFailures       prometheus.Counter
	sshOrphansReconciled prometheus.Counter
	slaBreaches          *prometheus.CounterVec

	logArchiveLastSuccess prometheus.Gauge
	logArchiveFailures    prometheus.Counter
	logArchivePending     prometheus.Gauge

	redactionDictSize     prometheus.Gauge
	redactionDictRebuilds *prometheus.CounterVec
}

// New builds a Registry with the Go runtime + process collectors and all
// Cronomicon metric vectors registered.
func New() *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m := &Registry{
		reg: reg,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cronomicon_http_requests_total",
			Help: "HTTP requests by method, matched route, and status class.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cronomicon_http_request_duration_seconds",
			Help:    "HTTP request latency by method and matched route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		runsStarted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronomicon_runs_started_total",
			Help: "Runs claimed by a runner (queued→running).",
		}),
		runsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cronomicon_runs_finished_total",
			Help: "Runs reaching a terminal status, by status.",
		}, []string{"status"}),
		logIngestBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronomicon_log_ingest_bytes_total",
			Help: "Total bytes of run-log chunks ingested.",
		}),
		logIngestChunks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronomicon_log_ingest_chunks_total",
			Help: "Total run-log chunks ingested.",
		}),
		schedulePublish: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronomicon_schedule_publishes_total",
			Help: "Successful schedule publishes to GitLab.",
		}),
		webhookSyncs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cronomicon_webhook_syncs_total",
			Help: "GitLab webhook-triggered syncs, by outcome.",
		}, []string{"outcome"}),
		backupLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cronomicon_backup_last_success_timestamp_seconds",
			Help: "Unix time of the last successful backup sweep (PP-H6). Alert when stale (e.g. now-this > 36h).",
		}),
		backupFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronomicon_backup_failures_total",
			Help: "Backup sweep failures (PP-H6 / PP-M4).",
		}),
		sshOrphansReconciled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronomicon_ssh_orphans_reconciled_total",
			Help: "SSH runs left 'running' by a crash and reconciled to failure=executor_lost (PP-H2).",
		}),
		slaBreaches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cronomicon_sla_breaches_total",
			Help: "SLA alerts raised, by kind: overdue (a run past its deadline) or missed (a fire that never happened). Neither kills or retries anything (SL).",
		}, []string{"kind"}),
		logArchiveLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cronomicon_log_archive_last_success_timestamp_seconds",
			Help: "Unix time of the last S3 log-archive sync tick that finished without error (SL-2). Alert when older than a few intervals.",
		}),
		logArchiveFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cronomicon_log_archive_failures_total",
			Help: "Log-archive sync ticks that ended with at least one failed upload (SL-2).",
		}),
		logArchivePending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cronomicon_log_archive_pending",
			Help: "Terminal runs whose log is not yet archived, as of the last sync tick (SL-2). Growing tick over tick means the interval is too long for the volume or the link is too slow.",
		}),
		redactionDictSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cronomicon_redaction_dictionary_size",
			Help: "Values in the process-wide redaction dictionary after its last rebuild (AM-4).",
		}),
		redactionDictRebuilds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cronomicon_redaction_dictionary_rebuilds_total",
			Help: "Rebuilds of the process-wide redaction dictionary by outcome: complete, partial (some encrypted values could not be decrypted — check the KEK), failed (database error; the previous dictionary was kept).",
		}, []string{"outcome"}),
	}
	reg.MustRegister(
		m.httpRequests, m.httpDuration, m.runsStarted, m.runsFinished,
		m.logIngestBytes, m.logIngestChunks, m.schedulePublish, m.webhookSyncs,
		m.backupLastSuccess, m.backupFailures, m.sshOrphansReconciled, m.slaBreaches,
		m.logArchiveLastSuccess, m.logArchiveFailures, m.logArchivePending,
		m.redactionDictSize, m.redactionDictRebuilds,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "cronomicon_active_runners",
			Help: "Runners currently online (computed on scrape).",
		}, activeRunners),
	)
	return m
}

// Handler returns the promhttp handler for this registry.
func (m *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// ObserveHTTP records one HTTP request's count and latency. route is the matched
// ServeMux pattern (low cardinality); status is the numeric code bucketed to its
// class (2xx/4xx/5xx) to keep label cardinality bounded.
func (m *Registry) ObserveHTTP(method, route string, status int, dur time.Duration) {
	if route == "" {
		route = "other"
	}
	m.httpRequests.WithLabelValues(method, route, statusClass(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(dur.Seconds())
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "other"
	}
}

// ── Package-level default registry + one-line recorders for subsystems ────────

var std = New()

// Default returns the process-wide registry served at /metrics.
func Default() *Registry { return std }

// RunStarted records a queued→running transition.
func RunStarted() { std.runsStarted.Inc() }

// RunFinished records a run reaching a terminal status.
func RunFinished(status string) { std.runsFinished.WithLabelValues(status).Inc() }

// AddLogIngest records ingested run-log volume.
func AddLogIngest(bytes int, chunks int) {
	std.logIngestBytes.Add(float64(bytes))
	std.logIngestChunks.Add(float64(chunks))
}

// SchedulePublish records a successful schedule publish.
func SchedulePublish() { std.schedulePublish.Inc() }

// WebhookSync records a webhook-triggered sync outcome ("ok" | "error").
func WebhookSync(outcome string) { std.webhookSyncs.WithLabelValues(outcome).Inc() }

// BackupSucceeded records the time of a successful backup sweep (PP-H6).
func BackupSucceeded(t time.Time) { std.backupLastSuccess.Set(float64(t.Unix())) }

// BackupFailed records a failed backup sweep (PP-H6 / PP-M4).
func BackupFailed() { std.backupFailures.Inc() }

// SSHOrphansReconciled records n SSH runs reconciled to executor_lost (PP-H2).
func SSHOrphansReconciled(n int) { std.sshOrphansReconciled.Add(float64(n)) }

// SLABreach records one SLA alert (SL). kind is "overdue" or "missed".
//
// A counter rather than a gauge: the question asked of Prometheus is "how often
// is this fleet late?", which is a rate. /metrics is deliberately outside the
// OpenAPI contract, so this needs no spec change.
func SLABreach(kind string) { std.slaBreaches.WithLabelValues(kind).Inc() }

// LogArchiveSucceeded records a log-archive sync tick that finished clean (SL-2).
func LogArchiveSucceeded(t time.Time) { std.logArchiveLastSuccess.Set(float64(t.Unix())) }

// LogArchiveFailed records a log-archive sync tick with at least one failure (SL-2).
func LogArchiveFailed() { std.logArchiveFailures.Inc() }

// LogArchivePending records the unarchived terminal-run count at tick end (SL-2).
func LogArchivePending(n int64) { std.logArchivePending.Set(float64(n)) }

// RedactionDictionaryRebuilt records one rebuild of the process-wide redaction
// dictionary (redactdict) and the size it now has.
func RedactionDictionaryRebuilt(outcome string, size int) {
	std.redactionDictRebuilds.WithLabelValues(outcome).Inc()
	std.redactionDictSize.Set(float64(size))
}
