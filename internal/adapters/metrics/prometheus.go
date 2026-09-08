// Package metrics provides adapters for the ports.MetricsExporter interface.
package metrics

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// PrometheusExporter exposes application metrics in the Prometheus format.
type PrometheusExporter struct {
	monitorStatus     *prometheus.GaugeVec
	monitorLatency    *prometheus.GaugeVec
	heartbeatsTotal   *prometheus.CounterVec
	notificationsSent *prometheus.CounterVec
	wsConnections     prometheus.Gauge
	monitorsActive    prometheus.Gauge
	busEventsDropped  *prometheus.CounterVec
	wsFramesDropped   prometheus.Counter
	insightsDuration  *prometheus.HistogramVec
	insightsCache     *prometheus.CounterVec
}

// NewPrometheusExporter creates a new Prometheus metrics exporter using promauto.
func NewPrometheusExporter() *PrometheusExporter {
	return &PrometheusExporter{
		insightsDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "phoenix_insights_stage_duration_seconds",
			Help:    "Insights stage latency, including database connection-pool wait",
			Buckets: prometheus.DefBuckets,
		}, []string{"stage"}),
		insightsCache: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "phoenix_insights_cache_total",
			Help: "Insights process-local cache hits, misses and coalesced requests",
		}, []string{"outcome"}),
		monitorStatus: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "phoenix_monitor_status",
			Help: "Current status of monitor (0=DOWN,1=UP,2=PENDING,3=MAINTENANCE)",
		}, []string{"monitor_id", "monitor_name", "type"}),
		monitorLatency: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "phoenix_monitor_latency_ms",
			Help: "Last check latency in milliseconds",
		}, []string{"monitor_id", "monitor_name"}),
		heartbeatsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "phoenix_heartbeats_total",
			Help: "Total heartbeats recorded",
		}, []string{"monitor_id", "status"}),
		notificationsSent: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "phoenix_notifications_sent_total",
			Help: "Total notifications sent by provider",
		}, []string{"provider", "status"}),
		wsConnections: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "phoenix_ws_connections_active",
			Help: "Current active WebSocket connections",
		}),
		monitorsActive: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "phoenix_monitors_active",
			Help: "Number of active monitors",
		}),
		// Both drop counters exist because these events used to vanish with no log
		// line and no metric: a backlogged WebSocket hub discarded UI events at the
		// bus buffer and again at the per-client buffer, so a lossy install was
		// indistinguishable from a quiet one. Any sustained increase here means
		// clients are NOT seeing the state the database holds.
		busEventsDropped: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "phoenix_eventbus_events_dropped_total",
			Help: "Events discarded because a subscriber's buffer was full",
		}, []string{"event_type"}),
		wsFramesDropped: promauto.NewCounter(prometheus.CounterOpts{
			Name: "phoenix_ws_frames_dropped_total",
			Help: "WebSocket frames discarded because a client's send buffer was full",
		}),
	}
}

func (p *PrometheusExporter) SetMonitorStatus(monitorID int64, monitorName, monitorType string, status float64) {
	p.monitorStatus.WithLabelValues(
		strconv.FormatInt(monitorID, 10), monitorName, monitorType,
	).Set(status)
}

func (p *PrometheusExporter) SetMonitorLatency(monitorID int64, monitorName string, latencyMs float64) {
	p.monitorLatency.WithLabelValues(
		strconv.FormatInt(monitorID, 10), monitorName,
	).Set(latencyMs)
}

func (p *PrometheusExporter) IncHeartbeat(monitorID int64, status string) {
	p.heartbeatsTotal.WithLabelValues(strconv.FormatInt(monitorID, 10), status).Inc()
}

func (p *PrometheusExporter) IncNotificationSent(provider, status string) {
	p.notificationsSent.WithLabelValues(provider, status).Inc()
}

// SetWSConnectionsActive publishes the live hub client count. The WebSocket
// hub calls this from AddClient/RemoveClient via SetDropMetrics.
func (p *PrometheusExporter) SetWSConnectionsActive(count float64) {
	p.wsConnections.Set(count)
}

func (p *PrometheusExporter) SetMonitorsActive(count float64) {
	p.monitorsActive.Set(count)
}

// IncBusEventDropped records one event dropped at an EventBus subscriber buffer.
// Satisfies the narrow drop-metrics interfaces in the eventbus package.
func (p *PrometheusExporter) IncBusEventDropped(eventType string) {
	p.busEventsDropped.WithLabelValues(eventType).Inc()
}

// IncWSFrameDropped records one frame dropped at a WebSocket client's buffer.
// Satisfies the narrow drop-metrics interface in the ws package.
func (p *PrometheusExporter) IncWSFrameDropped() {
	p.wsFramesDropped.Inc()
}

// ObserveInsightsStage records one completed read-model stage.
func (p *PrometheusExporter) ObserveInsightsStage(stage string, elapsed time.Duration) {
	p.insightsDuration.WithLabelValues(stage).Observe(elapsed.Seconds())
}

// IncInsightsCache records a hit, miss or shared in-flight calculation.
func (p *PrometheusExporter) IncInsightsCache(outcome string) {
	p.insightsCache.WithLabelValues(outcome).Inc()
}

// Handler returns the Prometheus HTTP handler.
func (p *PrometheusExporter) Handler() (any, error) {
	return promhttp.Handler(), nil
}

// Shutdown stops the metrics exporter.
func (p *PrometheusExporter) Shutdown(ctx context.Context) error {
	return nil
}

// Ensure PrometheusExporter implements MetricsExporter.
var _ ports.MetricsExporter = (*PrometheusExporter)(nil)
var _ ports.InsightsObserver = (*PrometheusExporter)(nil)
