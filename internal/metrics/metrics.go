// Package metrics provides Prometheus metrics for monitoring the P2P sync engine.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric labels
const (
	LabelPeerID    = "peer_id"
	LabelDirection = "direction" // "upload" or "download"
	LabelOperation = "operation"
	LabelStatus    = "status"
)

var (
	// Transfer metrics
	bytesTransferred = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "transfer",
			Name:      "bytes_total",
			Help:      "Total bytes transferred",
		},
		[]string{LabelDirection, LabelPeerID},
	)

	transferDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "p2psync",
			Subsystem: "transfer",
			Name:      "duration_seconds",
			Help:      "Transfer duration in seconds",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~16s
		},
		[]string{LabelDirection, LabelPeerID},
	)

	transferSpeed = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "p2psync",
			Subsystem: "transfer",
			Name:      "speed_bytes_per_second",
			Help:      "Current transfer speed in bytes per second",
		},
		[]string{LabelDirection},
	)

	// Chunk metrics
	chunksTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "chunks",
			Name:      "processed_total",
			Help:      "Total chunks processed",
		},
	)

	chunksDeduplicated = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "chunks",
			Name:      "deduplicated_total",
			Help:      "Total chunks deduplicated (already existed)",
		},
	)

	chunksStored = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "chunks",
			Name:      "stored_total",
			Help:      "Total new chunks stored",
		},
	)

	chunkSize = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "p2psync",
			Subsystem: "chunks",
			Name:      "size_bytes",
			Help:      "Distribution of chunk sizes",
			Buckets:   prometheus.ExponentialBuckets(256, 2, 10), // 256B to 128KB
		},
	)

	// Merkle tree metrics
	merkleTreeDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "p2psync",
			Subsystem: "merkle",
			Name:      "tree_depth",
			Help:      "Current Merkle tree depth",
		},
	)

	merkleNodesTotal = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "p2psync",
			Subsystem: "merkle",
			Name:      "nodes_total",
			Help:      "Total nodes in Merkle tree",
		},
	)

	merkleDiffDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "p2psync",
			Subsystem: "merkle",
			Name:      "diff_duration_seconds",
			Help:      "Duration of Merkle diff operations",
			Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 15),
		},
	)

	merkleDirtyNodes = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "p2psync",
			Subsystem: "merkle",
			Name:      "dirty_nodes_per_diff",
			Help:      "Number of dirty nodes found per diff operation",
			Buckets:   prometheus.ExponentialBuckets(1, 2, 12),
		},
	)

	// Delta sync metrics
	deltaBytesSkipped = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "delta",
			Name:      "bytes_skipped_total",
			Help:      "Bytes skipped due to delta sync (existing blocks)",
		},
	)

	deltaBytesTransferred = promauto.NewCounter(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "delta",
			Name:      "bytes_transferred_total",
			Help:      "Bytes transferred after delta compression",
		},
	)

	deltaCompressionRatio = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "p2psync",
			Subsystem: "delta",
			Name:      "compression_ratio",
			Help:      "Delta compression ratio (0-1, higher is better)",
			Buckets:   prometheus.LinearBuckets(0, 0.1, 11),
		},
	)

	// Connection metrics
	activeConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "p2psync",
			Subsystem: "connections",
			Name:      "active",
			Help:      "Number of active peer connections",
		},
	)

	connectionAttempts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "connections",
			Name:      "attempts_total",
			Help:      "Total connection attempts",
		},
		[]string{LabelStatus}, // "success", "failed"
	)

	holePunchAttempts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "nat",
			Name:      "hole_punch_attempts_total",
			Help:      "Total hole punch attempts",
		},
		[]string{LabelStatus},
	)

	// Sync operation metrics
	syncOperations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "sync",
			Name:      "operations_total",
			Help:      "Total sync operations",
		},
		[]string{LabelOperation, LabelStatus},
	)

	syncDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "p2psync",
			Subsystem: "sync",
			Name:      "duration_seconds",
			Help:      "Sync operation duration",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12),
		},
		[]string{LabelOperation},
	)

	// File system metrics
	filesWatched = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "p2psync",
			Subsystem: "watcher",
			Name:      "files_total",
			Help:      "Number of files being watched",
		},
	)

	fileEvents = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "p2psync",
			Subsystem: "watcher",
			Name:      "events_total",
			Help:      "File system events received",
		},
		[]string{"event_type"}, // "create", "modify", "delete"
	)
)

// Metrics provides the public interface for recording metrics.
type Metrics struct {
	uploadSpeedTracker   *speedTracker
	downloadSpeedTracker *speedTracker
}

// speedTracker tracks transfer speed using a sliding window.
type speedTracker struct {
	mu          sync.Mutex
	bytesTotal  int64
	lastUpdate  time.Time
	windowBytes []int64
	windowTimes []time.Time
	windowSize  int
}

func newSpeedTracker(windowSize int) *speedTracker {
	return &speedTracker{
		windowSize:  windowSize,
		windowBytes: make([]int64, 0, windowSize),
		windowTimes: make([]time.Time, 0, windowSize),
	}
}

func (st *speedTracker) Record(bytes int64) float64 {
	st.mu.Lock()
	defer st.mu.Unlock()

	now := time.Now()
	st.bytesTotal += bytes

	st.windowBytes = append(st.windowBytes, bytes)
	st.windowTimes = append(st.windowTimes, now)

	// Trim old entries
	if len(st.windowBytes) > st.windowSize {
		st.windowBytes = st.windowBytes[1:]
		st.windowTimes = st.windowTimes[1:]
	}

	// Calculate speed over window
	if len(st.windowTimes) < 2 {
		return 0
	}

	duration := st.windowTimes[len(st.windowTimes)-1].Sub(st.windowTimes[0])
	if duration == 0 {
		return 0
	}

	var totalBytes int64
	for _, b := range st.windowBytes {
		totalBytes += b
	}

	return float64(totalBytes) / duration.Seconds()
}

var (
	globalMetrics     *Metrics
	globalMetricsOnce sync.Once
)

// Get returns the global metrics instance.
func Get() *Metrics {
	globalMetricsOnce.Do(func() {
		globalMetrics = &Metrics{
			uploadSpeedTracker:   newSpeedTracker(10),
			downloadSpeedTracker: newSpeedTracker(10),
		}
	})
	return globalMetrics
}

// Transfer metrics

// RecordUpload records bytes uploaded.
func (m *Metrics) RecordUpload(peerID string, bytes int64, duration time.Duration) {
	bytesTransferred.WithLabelValues("upload", peerID).Add(float64(bytes))
	transferDuration.WithLabelValues("upload", peerID).Observe(duration.Seconds())

	speed := m.uploadSpeedTracker.Record(bytes)
	transferSpeed.WithLabelValues("upload").Set(speed)
}

// RecordDownload records bytes downloaded.
func (m *Metrics) RecordDownload(peerID string, bytes int64, duration time.Duration) {
	bytesTransferred.WithLabelValues("download", peerID).Add(float64(bytes))
	transferDuration.WithLabelValues("download", peerID).Observe(duration.Seconds())

	speed := m.downloadSpeedTracker.Record(bytes)
	transferSpeed.WithLabelValues("download").Set(speed)
}

// Chunk metrics

// RecordChunkProcessed records a processed chunk.
func (m *Metrics) RecordChunkProcessed(size int) {
	chunksTotal.Inc()
	chunkSize.Observe(float64(size))
}

// RecordChunkDeduplicated records a deduplicated chunk.
func (m *Metrics) RecordChunkDeduplicated() {
	chunksDeduplicated.Inc()
}

// RecordChunkStored records a newly stored chunk.
func (m *Metrics) RecordChunkStored() {
	chunksStored.Inc()
}

// Merkle tree metrics

// SetMerkleTreeStats updates Merkle tree statistics.
func (m *Metrics) SetMerkleTreeStats(depth int, nodeCount int) {
	merkleTreeDepth.Set(float64(depth))
	merkleNodesTotal.Set(float64(nodeCount))
}

// RecordMerkleDiff records a Merkle diff operation.
func (m *Metrics) RecordMerkleDiff(duration time.Duration, dirtyNodes int) {
	merkleDiffDuration.Observe(duration.Seconds())
	merkleDirtyNodes.Observe(float64(dirtyNodes))
}

// Delta sync metrics

// RecordDeltaSync records delta sync statistics.
func (m *Metrics) RecordDeltaSync(bytesSkipped, bytesTransferred int64) {
	deltaBytesSkipped.Add(float64(bytesSkipped))
	deltaBytesTransferred.Add(float64(bytesTransferred))

	total := bytesSkipped + bytesTransferred
	if total > 0 {
		ratio := float64(bytesSkipped) / float64(total)
		deltaCompressionRatio.Observe(ratio)
	}
}

// Connection metrics

// RecordConnectionAttempt records a connection attempt.
func (m *Metrics) RecordConnectionAttempt(success bool) {
	status := "failed"
	if success {
		status = "success"
	}
	connectionAttempts.WithLabelValues(status).Inc()
}

// SetActiveConnections sets the number of active connections.
func (m *Metrics) SetActiveConnections(count int) {
	activeConnections.Set(float64(count))
}

// RecordHolePunch records a hole punch attempt.
func (m *Metrics) RecordHolePunch(success bool) {
	status := "failed"
	if success {
		status = "success"
	}
	holePunchAttempts.WithLabelValues(status).Inc()
}

// Sync operation metrics

// RecordSyncOperation records a sync operation.
func (m *Metrics) RecordSyncOperation(operation string, success bool, duration time.Duration) {
	status := "failed"
	if success {
		status = "success"
	}
	syncOperations.WithLabelValues(operation, status).Inc()
	syncDuration.WithLabelValues(operation).Observe(duration.Seconds())
}

// File watcher metrics

// SetFilesWatched sets the number of watched files.
func (m *Metrics) SetFilesWatched(count int) {
	filesWatched.Set(float64(count))
}

// RecordFileEvent records a file system event.
func (m *Metrics) RecordFileEvent(eventType string) {
	fileEvents.WithLabelValues(eventType).Inc()
}

// Server provides HTTP endpoint for metrics.
type Server struct {
	httpServer *http.Server
}

// NewServer creates a metrics server.
func NewServer(addr string) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	return &Server{
		httpServer: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}
}

// Start begins serving metrics.
func (s *Server) Start() error {
	return s.httpServer.ListenAndServe()
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() error {
	return s.httpServer.Close()
}

// Convenience functions

// RecordUpload is a convenience function using the global metrics.
func RecordUpload(peerID string, bytes int64, duration time.Duration) {
	Get().RecordUpload(peerID, bytes, duration)
}

// RecordDownload is a convenience function using the global metrics.
func RecordDownload(peerID string, bytes int64, duration time.Duration) {
	Get().RecordDownload(peerID, bytes, duration)
}

// RecordChunkProcessed is a convenience function using the global metrics.
func RecordChunkProcessed(size int) {
	Get().RecordChunkProcessed(size)
}

// RecordChunkDeduplicated is a convenience function using the global metrics.
func RecordChunkDeduplicated() {
	Get().RecordChunkDeduplicated()
}

// RecordChunkStored is a convenience function using the global metrics.
func RecordChunkStored() {
	Get().RecordChunkStored()
}

// SetMerkleTreeStats is a convenience function using the global metrics.
func SetMerkleTreeStats(depth int, nodeCount int) {
	Get().SetMerkleTreeStats(depth, nodeCount)
}

// RecordMerkleDiff is a convenience function using the global metrics.
func RecordMerkleDiff(duration time.Duration, dirtyNodes int) {
	Get().RecordMerkleDiff(duration, dirtyNodes)
}

// RecordDeltaSync is a convenience function using the global metrics.
func RecordDeltaSync(bytesSkipped, bytesTransferred int64) {
	Get().RecordDeltaSync(bytesSkipped, bytesTransferred)
}
