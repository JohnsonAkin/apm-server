// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package modelindexer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/elastic/beats/v7/libbeat/esleg/eslegclient"
	"github.com/elastic/beats/v7/libbeat/logp"

	"github.com/elastic/apm-server/elasticsearch"
	logs "github.com/elastic/apm-server/log"
	"github.com/elastic/apm-server/model"
)

const (
	logRateLimit = time.Minute
)

// ErrClosed is returned from methods of closed Indexers.
var ErrClosed = errors.New("model indexer closed")

// Indexer is a model.BatchProcessor which bulk indexes events as Elasticsearch documents.
//
// Indexer buffers events in their JSON encoding until either the accumulated buffer reaches
// `config.FlushBytes`, or `config.FlushInterval` elapses.
//
// Indexer fills a single bulk request buffer at a time to ensure bulk requests are optimally
// sized, avoiding sparse bulk requests as much as possible. After a bulk request is flushed,
// the next event added will wait for the next available bulk request buffer and repeat the
// process.
//
// Up to `config.MaxRequests` bulk requests may be flushing/active concurrently, to allow the
// server to make progress encoding while Elasticsearch is busy servicing flushed bulk requests.
type Indexer struct {
	eventsAdded  int64
	eventsActive int64
	eventsFailed int64
	config       Config
	logger       *logp.Logger
	available    chan *bulkIndexer
	g            errgroup.Group

	mu       sync.RWMutex
	closing  bool
	closed   chan struct{}
	activeMu sync.Mutex
	active   *bulkIndexer
	timer    *time.Timer
}

// Config holds configuration for Indexer.
type Config struct {
	// MaxRequests holds the maximum number of bulk index requests to execute concurrently.
	// The maximum memory usage of Indexer is thus approximately MaxRequests*FlushBytes.
	//
	// If MaxRequests is less than or equal to zero, the default of 10 will be used.
	MaxRequests int

	// FlushBytes holds the flush threshold in bytes.
	//
	// If FlushBytes is zero, the default of 5MB will be used.
	FlushBytes int

	// FlushInterval holds the flush threshold as a duration.
	//
	// If FlushInterval is zero, the default of 30 seconds will be used.
	FlushInterval time.Duration
}

// New returns a new Indexer that indexes events directly into data streams.
func New(client elasticsearch.Client, cfg Config) (*Indexer, error) {
	logger := logp.NewLogger("modelindexer", logs.WithRateLimit(logRateLimit))
	if cfg.MaxRequests <= 0 {
		cfg.MaxRequests = 10
	}
	if cfg.FlushBytes <= 0 {
		cfg.FlushBytes = 5 * 1024 * 1024
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 30 * time.Second
	}
	available := make(chan *bulkIndexer, cfg.MaxRequests)
	for i := 0; i < cfg.MaxRequests; i++ {
		available <- newBulkIndexer(client)
	}
	return &Indexer{
		config:    cfg,
		logger:    logger,
		available: available,
		closed:    make(chan struct{}),
	}, nil
}

// Close closes the indexer, first flushing any queued events.
//
// Close returns an error if any flush attempts during the indexer's
// lifetime returned an error. If ctx is cancelled, Close returns and
// any ongoing flush attempts are cancelled.
func (i *Indexer) Close(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.closing {
		i.closing = true

		// Close i.closed when ctx is cancelled,
		// unblock any ongoing flush attempts.
		done := make(chan struct{})
		defer close(done)
		go func() {
			defer close(i.closed)
			select {
			case <-done:
			case <-ctx.Done():
			}
		}()

		i.activeMu.Lock()
		defer i.activeMu.Unlock()
		if i.active != nil && i.timer.Stop() {
			i.flushActiveLocked(ctx)
		}
	}
	return i.g.Wait()
}

// Stats returns the bulk indexing stats.
func (i *Indexer) Stats() Stats {
	return Stats{
		Added:  atomic.LoadInt64(&i.eventsAdded),
		Active: atomic.LoadInt64(&i.eventsActive),
		Failed: atomic.LoadInt64(&i.eventsFailed),
	}
}

// ProcessBatch creates a document for each event in batch, and adds them to the
// Elasticsearch bulk indexer.
//
// If the indexer has been closed, ProcessBatch returns ErrClosed.
func (i *Indexer) ProcessBatch(ctx context.Context, batch *model.Batch) error {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.closing {
		return ErrClosed
	}
	for _, event := range *batch {
		if err := i.processEvent(ctx, &event); err != nil {
			return err
		}
	}
	return nil
}

func (i *Indexer) processEvent(ctx context.Context, event *model.APMEvent) error {
	r := getPooledReader()
	beatEvent := event.BeatEvent(ctx)
	if err := r.encoder.AddRaw(&beatEvent); err != nil {
		return err
	}

	r.indexBuilder.WriteString(event.DataStream.Type)
	r.indexBuilder.WriteByte('-')
	r.indexBuilder.WriteString(event.DataStream.Dataset)
	r.indexBuilder.WriteByte('-')
	r.indexBuilder.WriteString(event.DataStream.Namespace)
	index := r.indexBuilder.String()

	i.activeMu.Lock()
	defer i.activeMu.Unlock()
	if i.active == nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case i.active = <-i.available:
		}
		if i.timer == nil {
			i.timer = time.AfterFunc(
				i.config.FlushInterval,
				i.flushActive,
			)
		} else {
			i.timer.Reset(i.config.FlushInterval)
		}
	}

	if err := i.active.Add(elasticsearch.BulkIndexerItem{
		Index:  index,
		Action: "create",
		Body:   r,
	}); err != nil {
		return err
	}
	atomic.AddInt64(&i.eventsAdded, 1)
	atomic.AddInt64(&i.eventsActive, 1)

	if i.active.Len() >= i.config.FlushBytes {
		if i.timer.Stop() {
			i.flushActiveLocked(context.Background())
		}
	}
	return nil
}

func (i *Indexer) flushActive() {
	i.activeMu.Lock()
	defer i.activeMu.Unlock()
	i.flushActiveLocked(context.Background())
}

func (i *Indexer) flushActiveLocked(ctx context.Context) {
	// Create a child context which is cancelled when the context passed to i.Close is cancelled.
	flushed := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		select {
		case <-i.closed:
		case <-flushed:
		}
	}()
	bulkIndexer := i.active
	i.active = nil
	i.g.Go(func() error {
		defer close(flushed)
		err := i.flush(ctx, bulkIndexer)
		bulkIndexer.Reset()
		i.available <- bulkIndexer
		return err
	})
}

func (i *Indexer) flush(ctx context.Context, bulkIndexer *bulkIndexer) error {
	n := bulkIndexer.Items()
	if n == 0 {
		return nil
	}
	defer atomic.AddInt64(&i.eventsActive, -int64(n))
	resp, err := bulkIndexer.Flush(ctx)
	if err != nil {
		atomic.AddInt64(&i.eventsFailed, int64(n))
		i.logger.With(logp.Error(err)).Error("bulk indexing request failed")
		return err
	}
	var eventsFailed int64
	for _, item := range resp.Items {
		for _, info := range item {
			if info.Error.Type != "" || info.Status > 201 {
				eventsFailed++
				i.logger.Errorf(
					"failed to index event (%s): %s",
					info.Error.Type, info.Error.Reason,
				)
			}
		}
	}
	if eventsFailed > 0 {
		atomic.AddInt64(&i.eventsFailed, eventsFailed)
	}
	return nil
}

var pool sync.Pool

type pooledReader struct {
	buf          bytes.Buffer
	indexBuilder strings.Builder
	encoder      encoder
}

func getPooledReader() *pooledReader {
	if r, ok := pool.Get().(*pooledReader); ok {
		return r
	}
	r := &pooledReader{}
	r.encoder = eslegclient.NewJSONEncoder(&r.buf, false)
	return r
}

func (r *pooledReader) Read(p []byte) (int, error) {
	n, err := r.buf.Read(p)
	if err == io.EOF {
		// Release the reader back into the pool after it has been consumed.
		r.indexBuilder.Reset()
		r.encoder.Reset()
		pool.Put(r)
	}
	return n, err
}

type encoder interface {
	AddRaw(interface{}) error
	Reset()
}

// Stats holds bulk indexing statistics.
type Stats struct {
	// Active holds the active number of items waiting in the indexer's queue.
	Active int64

	// Added holds the number of items added to the indexer.
	Added int64

	// Failed holds the number of indexing operations that failed.
	Failed int64
}
