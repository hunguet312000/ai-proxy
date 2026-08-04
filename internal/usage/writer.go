package usage

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"literouter/internal/storage"
)

const (
	defaultUsageQueueSize     = 4096
	defaultUsageBatchSize     = 100
	defaultUsageFlushInterval = 250 * time.Millisecond
	usageWriteTimeout         = 5 * time.Second
)

type usageWriter struct {
	store         *storage.Store
	logger        func() *slog.Logger
	queue         chan storage.UsageEvent
	stop          chan struct{}
	done          chan struct{}
	batchSize     int
	flushInterval time.Duration

	lifecycle sync.RWMutex
	started   bool
	stopped   bool
	dropped   atomic.Uint64
}

func newUsageWriter(store *storage.Store, logger func() *slog.Logger, queueSize, batchSize int, flushInterval time.Duration) *usageWriter {
	if queueSize <= 0 {
		queueSize = defaultUsageQueueSize
	}
	if batchSize <= 0 {
		batchSize = defaultUsageBatchSize
	}
	if flushInterval <= 0 {
		flushInterval = defaultUsageFlushInterval
	}
	return &usageWriter{
		store: store, logger: logger, queue: make(chan storage.UsageEvent, queueSize),
		stop: make(chan struct{}), done: make(chan struct{}), batchSize: batchSize, flushInterval: flushInterval,
	}
}

func (writer *usageWriter) enqueue(event storage.UsageEvent) bool {
	if writer == nil || writer.store == nil {
		return false
	}
	writer.lifecycle.RLock()
	if writer.stopped {
		writer.lifecycle.RUnlock()
		return false
	}
	if !writer.started {
		writer.lifecycle.RUnlock()
		writer.lifecycle.Lock()
		if writer.stopped {
			writer.lifecycle.Unlock()
			return false
		}
		if !writer.started {
			writer.started = true
			go writer.run()
		}
		writer.lifecycle.Unlock()
		writer.lifecycle.RLock()
	}
	defer writer.lifecycle.RUnlock()
	select {
	case writer.queue <- event:
		return true
	default:
		dropped := writer.dropped.Add(1)
		if dropped == 1 || dropped&(dropped-1) == 0 {
			if logger := writer.logger(); logger != nil {
				logger.Warn("usage queue full; dropping analytics event", "dropped", dropped, "capacity", cap(writer.queue))
			}
		}
		return false
	}
}

func (writer *usageWriter) close(ctx context.Context) error {
	if writer == nil {
		return nil
	}
	writer.lifecycle.Lock()
	if writer.stopped {
		started := writer.started
		writer.lifecycle.Unlock()
		if !started {
			return nil
		}
		select {
		case <-writer.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	writer.stopped = true
	started := writer.started
	if started {
		close(writer.stop)
	}
	writer.lifecycle.Unlock()
	if !started {
		return nil
	}
	select {
	case <-writer.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (writer *usageWriter) run() {
	defer close(writer.done)
	ticker := time.NewTicker(writer.flushInterval)
	defer ticker.Stop()
	batch := make([]storage.UsageEvent, 0, writer.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), usageWriteTimeout)
		err := writer.store.InsertUsageEvents(ctx, batch)
		cancel()
		if err != nil {
			if logger := writer.logger(); logger != nil {
				logger.Warn("record usage batch", "events", len(batch), "error", err)
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case event := <-writer.queue:
			batch = append(batch, event)
			if len(batch) >= writer.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-writer.stop:
			for {
				select {
				case event := <-writer.queue:
					batch = append(batch, event)
					if len(batch) >= writer.batchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}
