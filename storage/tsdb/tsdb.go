// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tsdb

import (
	"context"
	"sync"
	"time"

	"github.com/alecthomas/units"
	"github.com/go-kit/kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// ErrNotReady is returned if the underlying storage is not ready yet.
var ErrNotReady = errors.New("TSDB not ready")

// ReadyStorage implements the Storage interface while allowing to set the actual
// storage at a later point in time.
type ReadyStorage struct {
	mtx sync.RWMutex
	a   *adapter
}

// Set the storage.
func (s *ReadyStorage) Set(db *tsdb.DB, startTimeMargin int64) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	s.a = &adapter{db: db, startTimeMargin: startTimeMargin}
}

// Get the storage.
func (s *ReadyStorage) Get() *tsdb.DB {
	if x := s.get(); x != nil {
		return x.db
	}
	return nil
}

func (s *ReadyStorage) get() *adapter {
	s.mtx.RLock()
	x := s.a
	s.mtx.RUnlock()
	return x
}

// StartTime implements the Storage interface.
func (s *ReadyStorage) StartTime() (int64, error) {
	if x := s.get(); x != nil {
		return x.StartTime()
	}
	return int64(model.Latest), ErrNotReady
}

// Querier implements the Storage interface.
func (s *ReadyStorage) Querier(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
	if x := s.get(); x != nil {
		return x.Querier(ctx, mint, maxt)
	}
	return nil, ErrNotReady
}

// Appender implements the Storage interface.
func (s *ReadyStorage) Appender() (storage.Appender, error) {
	if x := s.get(); x != nil {
		return x.Appender()
	}
	return nil, ErrNotReady
}

// Close implements the Storage interface.
func (s *ReadyStorage) Close() error {
	if x := s.Get(); x != nil {
		return x.Close()
	}
	return nil
}

// Adapter return an adapter as storage.Storage.
func Adapter(db *tsdb.DB, startTimeMargin int64) storage.Storage {
	return &adapter{db: db, startTimeMargin: startTimeMargin}
}

// adapter implements a storage.Storage around TSDB.
type adapter struct {
	db              *tsdb.DB
	startTimeMargin int64
}

// Options of the DB storage.
type Options struct {
	// The timestamp range of head blocks after which they get persisted.
	// It's the minimum duration of any persisted block.
	MinBlockDuration model.Duration

	// The maximum timestamp range of compacted blocks.
	MaxBlockDuration model.Duration

	// The maximum size of each WAL segment file.
	WALSegmentSize units.Base2Bytes

	// Duration for how long to retain data.
	RetentionDuration model.Duration

	// Maximum number of bytes to be retained.
	MaxBytes units.Base2Bytes

	// Disable creation and consideration of lockfile.
	NoLockfile bool

	// When true it disables the overlapping blocks check.
	// This in-turn enables vertical compaction and vertical query merge.
	AllowOverlappingBlocks bool

	// When true records in the WAL will be compressed.
	WALCompression bool
}

var (
	startTime   prometheus.GaugeFunc
	headMaxTime prometheus.GaugeFunc
	headMinTime prometheus.GaugeFunc
)

func registerMetrics(db *tsdb.DB, r prometheus.Registerer) {

	startTime = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "prometheus_tsdb_lowest_timestamp_seconds",
		Help: "Lowest timestamp value stored in the database.",
	}, func() float64 {
		bb := db.Blocks()
		if len(bb) == 0 {
			return float64(db.Head().MinTime()) / 1000
		}
		return float64(db.Blocks()[0].Meta().MinTime) / 1000
	})
	headMinTime = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "prometheus_tsdb_head_min_time_seconds",
		Help: "Minimum time bound of the head block.",
	}, func() float64 {
		return float64(db.Head().MinTime()) / 1000
	})
	headMaxTime = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "prometheus_tsdb_head_max_time_seconds",
		Help: "Maximum timestamp of the head block.",
	}, func() float64 {
		return float64(db.Head().MaxTime()) / 1000
	})

	if r != nil {
		r.MustRegister(
			startTime,
			headMaxTime,
			headMinTime,
		)
	}
}

// Open returns a new storage backed by a TSDB database that is configured for Prometheus.
func Open(path string, l log.Logger, r prometheus.Registerer, opts *Options) (*tsdb.DB, error) {
	if opts.MinBlockDuration > opts.MaxBlockDuration {
		opts.MaxBlockDuration = opts.MinBlockDuration
	}
	// Start with smallest block duration and create exponential buckets until the exceed the
	// configured maximum block duration.
	rngs := tsdb.ExponentialBlockRanges(int64(time.Duration(opts.MinBlockDuration).Seconds()*1000), 10, 3)

	for i, v := range rngs {
		if v > int64(time.Duration(opts.MaxBlockDuration).Seconds()*1000) {
			rngs = rngs[:i]
			break
		}
	}

	db, err := tsdb.Open(path, l, r, &tsdb.Options{
		WALSegmentSize:         int(opts.WALSegmentSize),
		RetentionDuration:      uint64(time.Duration(opts.RetentionDuration).Seconds() * 1000),
		MaxBytes:               int64(opts.MaxBytes),
		BlockRanges:            rngs,
		NoLockfile:             opts.NoLockfile,
		AllowOverlappingBlocks: opts.AllowOverlappingBlocks,
		WALCompression:         opts.WALCompression,
	})
	if err != nil {
		return nil, err
	}
	registerMetrics(db, r)

	return db, nil
}

// StartTime implements the Storage interface.
func (a adapter) StartTime() (int64, error) {
	var startTime int64

	if len(a.db.Blocks()) > 0 {
		startTime = a.db.Blocks()[0].Meta().MinTime
	} else {
		startTime = time.Now().Unix() * 1000
	}

	// Add a safety margin as it may take a few minutes for everything to spin up.
	return startTime + a.startTimeMargin, nil
}

func (a adapter) Querier(_ context.Context, mint, maxt int64) (storage.Querier, error) {
	q, err := a.db.Querier(mint, maxt)
	if err != nil {
		return nil, err
	}
	return querier{q: q}, nil
}

// Appender returns a new appender against the storage.
func (a adapter) Appender() (storage.Appender, error) {
	return appender{a: a.db.Appender()}, nil
}

// Close closes the storage and all its underlying resources.
func (a adapter) Close() error {
	return a.db.Close()
}

type querier struct {
	q storage.Querier
}

func (q querier) Select(p *storage.SelectParams, ms ...*labels.Matcher) (storage.SeriesSet, storage.Warnings, error) {
	set, ws, err := q.q.Select(p, ms...)
	if err != nil {
		return nil, ws, err
	}
	return seriesSet{set: set}, ws, nil
}

func (q querier) SelectSorted(p *storage.SelectParams, ms ...*labels.Matcher) (storage.SeriesSet, storage.Warnings, error) {
	set, ws, err := q.q.SelectSorted(p, ms...)
	if err != nil {
		return nil, ws, err
	}
	return seriesSet{set: set}, ws, nil
}

func (q querier) LabelValues(name string) ([]string, storage.Warnings, error) {
	return q.LabelValues(name)
}
func (q querier) LabelNames() ([]string, storage.Warnings, error) {
	return q.q.LabelNames()
}
func (q querier) Close() error { return q.q.Close() }

type seriesSet struct {
	set storage.SeriesSet
}

func (s seriesSet) Next() bool         { return s.set.Next() }
func (s seriesSet) Err() error         { return s.set.Err() }
func (s seriesSet) At() storage.Series { return series{s: s.set.At()} }

type series struct {
	s storage.Series
}

func (s series) Labels() labels.Labels       { return s.s.Labels() }
func (s series) Iterator() chunkenc.Iterator { return s.s.Iterator() }

type appender struct {
	a tsdb.Appender
}

func (a appender) Add(lset labels.Labels, t int64, v float64) (uint64, error) {
	ref, err := a.a.Add(lset, t, v)

	switch errors.Cause(err) {
	case tsdb.ErrNotFound:
		return 0, storage.ErrNotFound
	case tsdb.ErrOutOfOrderSample:
		return 0, storage.ErrOutOfOrderSample
	case tsdb.ErrAmendSample:
		return 0, storage.ErrDuplicateSampleForTimestamp
	case tsdb.ErrOutOfBounds:
		return 0, storage.ErrOutOfBounds
	}
	return ref, err
}

func (a appender) AddFast(_ labels.Labels, ref uint64, t int64, v float64) error {
	err := a.a.AddFast(ref, t, v)

	switch errors.Cause(err) {
	case tsdb.ErrNotFound:
		return storage.ErrNotFound
	case tsdb.ErrOutOfOrderSample:
		return storage.ErrOutOfOrderSample
	case tsdb.ErrAmendSample:
		return storage.ErrDuplicateSampleForTimestamp
	case tsdb.ErrOutOfBounds:
		return storage.ErrOutOfBounds
	}
	return err
}

func (a appender) Commit() error   { return a.a.Commit() }
func (a appender) Rollback() error { return a.a.Rollback() }
