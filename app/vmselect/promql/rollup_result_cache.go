package promql

import (
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fs"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/memory"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/querytracer"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/stringsutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/workingsetcache"
	"github.com/VictoriaMetrics/fastcache"
	"github.com/VictoriaMetrics/metrics"
	"github.com/VictoriaMetrics/metricsql"
)

var (
	cacheTimestampOffset = flag.Duration("search.cacheTimestampOffset", 5*time.Minute, "The maximum duration since the current time for response data, "+
		"which is always queried from the original raw data, without using the response cache. Increase this value if you see gaps in responses "+
		"due to time synchronization issues between VictoriaMetrics and data sources. See also -search.disableAutoCacheReset")
	disableAutoCacheReset = flag.Bool("search.disableAutoCacheReset", false, "Whether to disable automatic response cache reset if a sample with timestamp "+
		"outside -search.cacheTimestampOffset is inserted into VictoriaMetrics")
)

// ResetRollupResultCacheIfNeeded resets rollup result cache if mrs contains timestamps outside `now - search.cacheTimestampOffset`.
func ResetRollupResultCacheIfNeeded(mrs []storage.MetricRow) {
	if *disableAutoCacheReset {
		// Do not reset response cache if -search.disableAutoCacheReset is set.
		// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/1570 .
		return
	}
	checkRollupResultCacheResetOnce.Do(func() {
		rollupResultResetMetricRowSample.Store(&storage.MetricRow{})
		go checkRollupResultCacheReset()
	})
	minTimestamp := int64(fasttime.UnixTimestamp()*1000) - cacheTimestampOffset.Milliseconds() + checkRollupResultCacheResetInterval.Milliseconds()
	needCacheReset := false
	for i := range mrs {
		if mrs[i].Timestamp < minTimestamp {
			var mr storage.MetricRow
			mr.CopyFrom(&mrs[i])
			rollupResultResetMetricRowSample.Store(&mr)
			needCacheReset = true
			break
		}
	}
	if needCacheReset {
		// Do not call ResetRollupResultCache() here, since it may be heavy when frequently called.
		atomic.StoreUint32(&needRollupResultCacheReset, 1)
	}
}

func checkRollupResultCacheReset() {
	for {
		time.Sleep(checkRollupResultCacheResetInterval)
		if atomic.SwapUint32(&needRollupResultCacheReset, 0) > 0 {
			mr := rollupResultResetMetricRowSample.Load()
			d := int64(fasttime.UnixTimestamp()*1000) - mr.Timestamp - cacheTimestampOffset.Milliseconds()
			logger.Warnf("resetting rollup result cache because the metric %s has a timestamp older than -search.cacheTimestampOffset=%s by %.3fs",
				mr.String(), cacheTimestampOffset, float64(d)/1e3)
			ResetRollupResultCache()
		}
	}
}

const checkRollupResultCacheResetInterval = 5 * time.Second

var needRollupResultCacheReset uint32
var checkRollupResultCacheResetOnce sync.Once
var rollupResultResetMetricRowSample atomic.Pointer[storage.MetricRow]

var rollupResultCacheV = &rollupResultCache{
	c: workingsetcache.New(1024 * 1024), // This is a cache for testing.
}
var rollupResultCachePath string

func getRollupResultCacheSize() int {
	rollupResultCacheSizeOnce.Do(func() {
		n := memory.Allowed() / 16
		if n <= 0 {
			n = 1024 * 1024
		}
		rollupResultCacheSize = n
	})
	return rollupResultCacheSize
}

var (
	rollupResultCacheSize     int
	rollupResultCacheSizeOnce sync.Once
)

// InitRollupResultCache initializes the rollupResult cache
//
// if cachePath is empty, then the cache isn't stored to persistent disk.
//
// ResetRollupResultCache must be called when the cache must be reset.
// StopRollupResultCache must be called when the cache isn't needed anymore.
func InitRollupResultCache(cachePath string) {
	rollupResultCachePath = cachePath
	startTime := time.Now()
	cacheSize := getRollupResultCacheSize()
	var c *workingsetcache.Cache
	if len(rollupResultCachePath) > 0 {
		logger.Infof("loading rollupResult cache from %q...", rollupResultCachePath)
		c = workingsetcache.Load(rollupResultCachePath, cacheSize)
		mustLoadRollupResultCacheKeyPrefix(rollupResultCachePath)
	} else {
		c = workingsetcache.New(cacheSize)
		rollupResultCacheKeyPrefix = newRollupResultCacheKeyPrefix()
	}
	if *disableCache {
		c.Reset()
	}

	stats := &fastcache.Stats{}
	var statsLock sync.Mutex
	var statsLastUpdate uint64
	fcs := func() *fastcache.Stats {
		statsLock.Lock()
		defer statsLock.Unlock()

		if fasttime.UnixTimestamp()-statsLastUpdate < 2 {
			return stats
		}
		var fcs fastcache.Stats
		c.UpdateStats(&fcs)
		stats = &fcs
		statsLastUpdate = fasttime.UnixTimestamp()
		return stats
	}
	if len(rollupResultCachePath) > 0 {
		logger.Infof("loaded rollupResult cache from %q in %.3f seconds; entriesCount: %d, sizeBytes: %d",
			rollupResultCachePath, time.Since(startTime).Seconds(), fcs().EntriesCount, fcs().BytesSize)
	}

	// Use metrics.GetOrCreateGauge instead of metrics.NewGauge,
	// so InitRollupResultCache+StopRollupResultCache could be called multiple times in tests.
	// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/2406
	metrics.GetOrCreateGauge(`vm_cache_entries{type="promql/rollupResult"}`, func() float64 {
		return float64(fcs().EntriesCount)
	})
	metrics.GetOrCreateGauge(`vm_cache_size_bytes{type="promql/rollupResult"}`, func() float64 {
		return float64(fcs().BytesSize)
	})
	metrics.GetOrCreateGauge(`vm_cache_size_max_bytes{type="promql/rollupResult"}`, func() float64 {
		return float64(fcs().MaxBytesSize)
	})
	metrics.GetOrCreateGauge(`vm_cache_requests_total{type="promql/rollupResult"}`, func() float64 {
		return float64(fcs().GetCalls)
	})
	metrics.GetOrCreateGauge(`vm_cache_misses_total{type="promql/rollupResult"}`, func() float64 {
		return float64(fcs().Misses)
	})

	rollupResultCacheV = &rollupResultCache{
		c: c,
	}
}

// StopRollupResultCache closes the rollupResult cache.
func StopRollupResultCache() {
	if len(rollupResultCachePath) == 0 {
		rollupResultCacheV.c.Stop()
		rollupResultCacheV.c = nil
		return
	}
	logger.Infof("saving rollupResult cache to %q...", rollupResultCachePath)
	startTime := time.Now()
	if err := rollupResultCacheV.c.Save(rollupResultCachePath); err != nil {
		logger.Errorf("cannot save rollupResult cache at %q: %s", rollupResultCachePath, err)
		return
	}
	mustSaveRollupResultCacheKeyPrefix(rollupResultCachePath)
	var fcs fastcache.Stats
	rollupResultCacheV.c.UpdateStats(&fcs)
	rollupResultCacheV.c.Stop()
	rollupResultCacheV.c = nil
	logger.Infof("saved rollupResult cache to %q in %.3f seconds; entriesCount: %d, sizeBytes: %d",
		rollupResultCachePath, time.Since(startTime).Seconds(), fcs.EntriesCount, fcs.BytesSize)
}

type rollupResultCache struct {
	c *workingsetcache.Cache
}

var rollupResultCacheResets = metrics.NewCounter(`vm_cache_resets_total{type="promql/rollupResult"}`)

// ResetRollupResultCache resets rollup result cache.
func ResetRollupResultCache() {
	rollupResultCacheResets.Inc()
	atomic.AddUint64(&rollupResultCacheKeyPrefix, 1)
	logger.Infof("rollupResult cache has been cleared")
}

func (rrc *rollupResultCache) GetInstantValues(qt *querytracer.Tracer, expr metricsql.Expr, window, step int64, etfss [][]storage.TagFilter) []*timeseries {
	if qt.Enabled() {
		query := string(expr.AppendString(nil))
		query = stringsutil.LimitStringLen(query, 300)
		qt = qt.NewChild("rollup cache get instant values: query=%s, window=%d, step=%d", query, window, step)
		defer qt.Done()
	}

	// Obtain instant values from the cache
	bb := bbPool.Get()
	defer bbPool.Put(bb)

	bb.B = marshalRollupResultCacheKeyForInstantValues(bb.B[:0], expr, window, step, etfss)
	tss, ok := rrc.getSeriesFromCache(qt, bb.B)
	if !ok || len(tss) == 0 {
		return nil
	}
	assertInstantValues(tss)
	qt.Printf("found %d series for time=%s", len(tss), storage.TimestampToHumanReadableFormat(tss[0].Timestamps[0]))
	return tss
}

func (rrc *rollupResultCache) PutInstantValues(qt *querytracer.Tracer, expr metricsql.Expr, window, step int64, etfss [][]storage.TagFilter, tss []*timeseries) {
	if qt.Enabled() {
		query := string(expr.AppendString(nil))
		query = stringsutil.LimitStringLen(query, 300)
		startStr := ""
		if len(tss) > 0 {
			startStr = storage.TimestampToHumanReadableFormat(tss[0].Timestamps[0])
		}
		qt = qt.NewChild("rollup cache put instant values: query=%s, window=%d, step=%d, series=%d, time=%s", query, window, step, len(tss), startStr)
		defer qt.Done()
	}
	if len(tss) == 0 {
		qt.Printf("do not cache empty series list")
		return
	}

	assertInstantValues(tss)

	bb := bbPool.Get()
	defer bbPool.Put(bb)

	bb.B = marshalRollupResultCacheKeyForInstantValues(bb.B[:0], expr, window, step, etfss)
	_ = rrc.putSeriesToCache(qt, bb.B, step, tss)
}

func (rrc *rollupResultCache) DeleteInstantValues(qt *querytracer.Tracer, expr metricsql.Expr, window, step int64, etfss [][]storage.TagFilter) {
	bb := bbPool.Get()
	defer bbPool.Put(bb)

	bb.B = marshalRollupResultCacheKeyForInstantValues(bb.B[:0], expr, window, step, etfss)
	if !rrc.putSeriesToCache(qt, bb.B, step, nil) {
		logger.Panicf("BUG: cannot store zero series to cache")
	}

	if qt.Enabled() {
		query := string(expr.AppendString(nil))
		query = stringsutil.LimitStringLen(query, 300)
		qt.Printf("rollup result cache delete instant values: query=%s, window=%d, step=%d", query, window, step)
	}
}

func (rrc *rollupResultCache) GetSeries(qt *querytracer.Tracer, ec *EvalConfig, expr metricsql.Expr, window int64) (tss []*timeseries, newStart int64) {
	if qt.Enabled() {
		query := string(expr.AppendString(nil))
		query = stringsutil.LimitStringLen(query, 300)
		qt = qt.NewChild("rollup cache get series: query=%s, timeRange=%s, window=%d, step=%d", query, ec.timeRangeString(), window, ec.Step)
		defer qt.Done()
	}
	if !ec.mayCache() {
		qt.Printf("do not fetch series from cache, since it is disabled in the current context")
		return nil, ec.Start
	}

	// Obtain tss from the cache.
	bb := bbPool.Get()
	defer bbPool.Put(bb)

	bb.B = marshalRollupResultCacheKeyForSeries(bb.B[:0], expr, window, ec.Step, ec.EnforcedTagFilterss)
	metainfoBuf := rrc.c.Get(nil, bb.B)
	if len(metainfoBuf) == 0 {
		qt.Printf("nothing found")
		return nil, ec.Start
	}
	var mi rollupResultCacheMetainfo
	if err := mi.Unmarshal(metainfoBuf); err != nil {
		logger.Panicf("BUG: cannot unmarshal rollupResultCacheMetainfo: %s; it looks like it was improperly saved", err)
	}
	key := mi.GetBestKey(ec.Start, ec.End)
	if key.prefix == 0 && key.suffix == 0 {
		qt.Printf("nothing found on the timeRange")
		return nil, ec.Start
	}

	var ok bool
	bb.B = key.Marshal(bb.B[:0])
	tss, ok = rrc.getSeriesFromCache(qt, bb.B)
	if !ok {
		mi.RemoveKey(key)
		metainfoBuf = mi.Marshal(metainfoBuf[:0])
		bb.B = marshalRollupResultCacheKeyForSeries(bb.B[:0], expr, window, ec.Step, ec.EnforcedTagFilterss)
		rrc.c.Set(bb.B, metainfoBuf)
		return nil, ec.Start
	}

	// Extract values for the matching timestamps
	timestamps := tss[0].Timestamps
	i := 0
	for i < len(timestamps) && timestamps[i] < ec.Start {
		i++
	}
	if i == len(timestamps) {
		qt.Printf("no datapoints found in the cached series on the given timeRange")
		return nil, ec.Start
	}
	if timestamps[i] != ec.Start {
		qt.Printf("cached series don't cover the given timeRange")
		return nil, ec.Start
	}

	j := len(timestamps) - 1
	for j >= 0 && timestamps[j] > ec.End {
		j--
	}
	j++
	if j <= i {
		qt.Printf("no matching samples for the given timeRange")
		return nil, ec.Start
	}

	for _, ts := range tss {
		ts.Timestamps = ts.Timestamps[i:j]
		ts.Values = ts.Values[i:j]
	}

	timestamps = tss[0].Timestamps
	newStart = timestamps[len(timestamps)-1] + ec.Step
	if qt.Enabled() {
		startString := storage.TimestampToHumanReadableFormat(ec.Start)
		endString := storage.TimestampToHumanReadableFormat(newStart - ec.Step)
		qt.Printf("return %d series on a timeRange=[%s..%s]", len(tss), startString, endString)
	}
	return tss, newStart
}

var resultBufPool bytesutil.ByteBufferPool

func (rrc *rollupResultCache) PutSeries(qt *querytracer.Tracer, ec *EvalConfig, expr metricsql.Expr, window int64, tss []*timeseries) {
	if qt.Enabled() {
		query := string(expr.AppendString(nil))
		query = stringsutil.LimitStringLen(query, 300)
		qt = qt.NewChild("rollup cache put series: query=%s, timeRange=%s, step=%d, window=%d, series=%d", query, ec.timeRangeString(), ec.Step, window, len(tss))
		defer qt.Done()
	}
	if !ec.mayCache() {
		qt.Printf("do not store series to cache, since it is disabled in the current context")
		return
	}
	if len(tss) == 0 {
		qt.Printf("do not store empty series list")
		return
	}

	// Remove values up to currentTime - step - cacheTimestampOffset,
	// since these values may be added later.
	timestamps := tss[0].Timestamps
	deadline := (time.Now().UnixNano() / 1e6) - ec.Step - cacheTimestampOffset.Milliseconds()
	i := len(timestamps) - 1
	for i >= 0 && timestamps[i] > deadline {
		i--
	}
	i++
	if i == 0 {
		// Nothing to store in the cache.
		qt.Printf("nothing to store in the cache, since all the points have timestamps bigger than %d", deadline)
		return
	}
	if i < len(timestamps) {
		timestamps = timestamps[:i]
		// Make a copy of tss and remove unfit values
		rvs := copyTimeseriesShallow(tss)
		for _, ts := range rvs {
			ts.Timestamps = ts.Timestamps[:i]
			ts.Values = ts.Values[:i]
		}
		tss = rvs
	}

	// Store tss in the cache.
	metainfoKey := bbPool.Get()
	defer bbPool.Put(metainfoKey)
	metainfoBuf := bbPool.Get()
	defer bbPool.Put(metainfoBuf)

	metainfoKey.B = marshalRollupResultCacheKeyForSeries(metainfoKey.B[:0], expr, window, ec.Step, ec.EnforcedTagFilterss)
	metainfoBuf.B = rrc.c.Get(metainfoBuf.B[:0], metainfoKey.B)
	var mi rollupResultCacheMetainfo
	if len(metainfoBuf.B) > 0 {
		if err := mi.Unmarshal(metainfoBuf.B); err != nil {
			logger.Panicf("BUG: cannot unmarshal rollupResultCacheMetainfo: %s; it looks like it was improperly saved", err)
		}
	}
	start := timestamps[0]
	end := timestamps[len(timestamps)-1]
	if mi.CoversTimeRange(start, end) {
		if qt.Enabled() {
			startString := storage.TimestampToHumanReadableFormat(start)
			endString := storage.TimestampToHumanReadableFormat(end)
			qt.Printf("series on the given timeRange=[%s..%s] already exist in the cache", startString, endString)
		}
		return
	}

	var key rollupResultCacheKey
	key.prefix = rollupResultCacheKeyPrefix
	key.suffix = atomic.AddUint64(&rollupResultCacheKeySuffix, 1)

	bb := bbPool.Get()
	bb.B = key.Marshal(bb.B[:0])
	ok := rrc.putSeriesToCache(qt, bb.B, ec.Step, tss)
	bbPool.Put(bb)
	if !ok {
		return
	}

	mi.AddKey(key, timestamps[0], timestamps[len(timestamps)-1])
	metainfoBuf.B = mi.Marshal(metainfoBuf.B[:0])
	rrc.c.Set(metainfoKey.B, metainfoBuf.B)
}

var (
	rollupResultCacheKeyPrefix uint64
	rollupResultCacheKeySuffix = uint64(time.Now().UnixNano())
)

func (rrc *rollupResultCache) getSeriesFromCache(qt *querytracer.Tracer, key []byte) ([]*timeseries, bool) {
	compressedResultBuf := resultBufPool.Get()
	compressedResultBuf.B = rrc.c.GetBig(compressedResultBuf.B[:0], key)
	if len(compressedResultBuf.B) == 0 {
		qt.Printf("nothing found in the cache")
		resultBufPool.Put(compressedResultBuf)
		return nil, false
	}
	qt.Printf("load compressed entry from cache with size %d bytes", len(compressedResultBuf.B))
	// Decompress into newly allocated byte slice, since tss returned from unmarshalTimeseriesFast
	// refers to the byte slice, so it cannot be re-used.
	resultBuf, err := encoding.DecompressZSTD(nil, compressedResultBuf.B)
	if err != nil {
		logger.Panicf("BUG: cannot decompress resultBuf from rollupResultCache: %s; it looks like it was improperly saved", err)
	}
	resultBufPool.Put(compressedResultBuf)
	qt.Printf("unpack the entry into %d bytes", len(resultBuf))
	tss, err := unmarshalTimeseriesFast(resultBuf)
	if err != nil {
		logger.Panicf("BUG: cannot unmarshal timeseries from rollupResultCache: %s; it looks like it was improperly saved", err)
	}
	qt.Printf("unmarshal %d series", len(tss))
	return tss, true
}

func (rrc *rollupResultCache) putSeriesToCache(qt *querytracer.Tracer, key []byte, step int64, tss []*timeseries) bool {
	maxMarshaledSize := getRollupResultCacheSize() / 4
	resultBuf := resultBufPool.Get()
	defer resultBufPool.Put(resultBuf)
	resultBuf.B = marshalTimeseriesFast(resultBuf.B[:0], tss, maxMarshaledSize, step)
	if len(resultBuf.B) == 0 {
		tooBigRollupResults.Inc()
		qt.Printf("cannot store %d series in the cache, since they would occupy more than %d bytes", len(tss), maxMarshaledSize)
		return false
	}
	qt.Printf("marshal %d series into %d bytes", len(tss), len(resultBuf.B))
	compressedResultBuf := resultBufPool.Get()
	defer resultBufPool.Put(compressedResultBuf)
	compressedResultBuf.B = encoding.CompressZSTDLevel(compressedResultBuf.B[:0], resultBuf.B, 1)
	qt.Printf("compress %d bytes into %d bytes", len(resultBuf.B), len(compressedResultBuf.B))

	rrc.c.SetBig(key, compressedResultBuf.B)
	qt.Printf("store %d bytes in the cache", len(compressedResultBuf.B))
	return true
}

func newRollupResultCacheKeyPrefix() uint64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// do not use logger.Panicf, since it isn't initialized yet.
		panic(fmt.Errorf("FATAL: cannot read random data for rollupResultCacheKeyPrefix: %w", err))
	}
	return encoding.UnmarshalUint64(buf[:])
}

func mustLoadRollupResultCacheKeyPrefix(path string) {
	path = path + ".key.prefix"
	if !fs.IsPathExist(path) {
		rollupResultCacheKeyPrefix = newRollupResultCacheKeyPrefix()
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Errorf("cannot load %s: %s; reset rollupResult cache", path, err)
		rollupResultCacheKeyPrefix = newRollupResultCacheKeyPrefix()
		return
	}
	if len(data) != 8 {
		logger.Errorf("unexpected size of %s; want 8 bytes; got %d bytes; reset rollupResult cache", path, len(data))
		rollupResultCacheKeyPrefix = newRollupResultCacheKeyPrefix()
		return
	}
	rollupResultCacheKeyPrefix = encoding.UnmarshalUint64(data)
}

func mustSaveRollupResultCacheKeyPrefix(path string) {
	path = path + ".key.prefix"
	data := encoding.MarshalUint64(nil, rollupResultCacheKeyPrefix)
	fs.MustWriteAtomic(path, data, true)
}

var tooBigRollupResults = metrics.NewCounter("vm_too_big_rollup_results_total")

// Increment this value every time the format of the cache changes.
const rollupResultCacheVersion = 10

const (
	rollupResultCacheTypeSeries        = 0
	rollupResultCacheTypeInstantValues = 1
)

func marshalRollupResultCacheKeyForSeries(dst []byte, expr metricsql.Expr, window, step int64, etfs [][]storage.TagFilter) []byte {
	dst = append(dst, rollupResultCacheVersion)
	dst = encoding.MarshalUint64(dst, rollupResultCacheKeyPrefix)
	dst = append(dst, rollupResultCacheTypeSeries)
	dst = encoding.MarshalInt64(dst, window)
	dst = encoding.MarshalInt64(dst, step)
	dst = marshalTagFiltersForRollupResultCacheKey(dst, etfs)
	dst = expr.AppendString(dst)
	return dst
}

func marshalRollupResultCacheKeyForInstantValues(dst []byte, expr metricsql.Expr, window, step int64, etfs [][]storage.TagFilter) []byte {
	dst = append(dst, rollupResultCacheVersion)
	dst = encoding.MarshalUint64(dst, rollupResultCacheKeyPrefix)
	dst = append(dst, rollupResultCacheTypeInstantValues)
	dst = encoding.MarshalInt64(dst, window)
	dst = encoding.MarshalInt64(dst, step)
	dst = marshalTagFiltersForRollupResultCacheKey(dst, etfs)
	dst = expr.AppendString(dst)
	return dst
}

func marshalTagFiltersForRollupResultCacheKey(dst []byte, etfs [][]storage.TagFilter) []byte {
	for i, etf := range etfs {
		for _, f := range etf {
			dst = f.Marshal(dst)
		}
		if i+1 < len(etfs) {
			dst = append(dst, '|')
		}
	}
	return dst
}

// mergeTimeseries concatenates b with a and returns the result.
//
// Preconditions:
// - a mustn't intersect with b by timestamps.
// - a timestamps must be smaller than b timestamps.
//
// Postconditions:
// - a and b cannot be used after returning from the call.
func mergeTimeseries(qt *querytracer.Tracer, a, b []*timeseries, bStart int64, ec *EvalConfig) ([]*timeseries, error) {
	qt = qt.NewChild("merge series len(a)=%d, len(b)=%d", len(a), len(b))
	defer qt.Done()

	sharedTimestamps := ec.getSharedTimestamps()
	if bStart == ec.Start {
		// Nothing to merge - b covers all the time range.
		// Verify b is correct.
		for _, tsB := range b {
			tsB.denyReuse = true
			tsB.Timestamps = sharedTimestamps
			if len(tsB.Values) != len(tsB.Timestamps) {
				logger.Panicf("BUG: unexpected number of values in b; got %d; want %d", len(tsB.Values), len(tsB.Timestamps))
			}
		}
		return b, nil
	}

	m := make(map[string]*timeseries, len(a))
	bb := bbPool.Get()
	defer bbPool.Put(bb)
	for _, ts := range a {
		bb.B = marshalMetricNameSorted(bb.B[:0], &ts.MetricName)
		if _, ok := m[string(bb.B)]; ok {
			return nil, fmt.Errorf("duplicate series found: %s", &ts.MetricName)
		}
		m[string(bb.B)] = ts
	}

	rvs := make([]*timeseries, 0, len(a))
	for _, tsB := range b {
		var tmp timeseries
		tmp.denyReuse = true
		tmp.Timestamps = sharedTimestamps
		tmp.Values = make([]float64, 0, len(tmp.Timestamps))
		tmp.MetricName.MoveFrom(&tsB.MetricName)

		bb.B = marshalMetricNameSorted(bb.B[:0], &tmp.MetricName)
		k := string(bb.B)
		tsA := m[k]
		if tsA == nil {
			tStart := ec.Start
			for tStart < bStart {
				tmp.Values = append(tmp.Values, nan)
				tStart += ec.Step
			}
		} else {
			tmp.Values = append(tmp.Values, tsA.Values...)
			delete(m, k)
		}
		tmp.Values = append(tmp.Values, tsB.Values...)
		if len(tmp.Values) != len(tmp.Timestamps) {
			logger.Panicf("BUG: unexpected values after merging new values; got %d; want %d; len(a.Values)=%d; len(b.Values)=%d",
				len(tmp.Values), len(tmp.Timestamps), len(tsA.Values), len(tsB.Values))
		}
		rvs = append(rvs, &tmp)
	}

	// Copy the remaining timeseries from m.
	for _, tsA := range m {
		var tmp timeseries
		tmp.denyReuse = true
		tmp.Timestamps = sharedTimestamps
		tmp.Values = make([]float64, 0, len(tmp.Timestamps))
		tmp.MetricName.MoveFrom(&tsA.MetricName)
		tmp.Values = append(tmp.Values, tsA.Values...)

		tStart := bStart
		for tStart <= ec.End {
			tmp.Values = append(tmp.Values, nan)
			tStart += ec.Step
		}
		if len(tmp.Values) != len(tmp.Timestamps) {
			logger.Panicf("BUG: unexpected values in the result after adding cached values; got %d; want %d", len(tmp.Values), len(tmp.Timestamps))
		}
		rvs = append(rvs, &tmp)
	}
	qt.Printf("resulting series=%d", len(rvs))
	return rvs, nil
}

type rollupResultCacheMetainfo struct {
	entries []rollupResultCacheMetainfoEntry
}

func (mi *rollupResultCacheMetainfo) Marshal(dst []byte) []byte {
	dst = encoding.MarshalUint32(dst, uint32(len(mi.entries)))
	for i := range mi.entries {
		dst = mi.entries[i].Marshal(dst)
	}
	return dst
}

func (mi *rollupResultCacheMetainfo) Unmarshal(src []byte) error {
	if len(src) < 4 {
		return fmt.Errorf("cannot unmarshal len(etries) from %d bytes; need at least %d bytes", len(src), 4)
	}
	entriesLen := int(encoding.UnmarshalUint32(src))
	src = src[4:]
	if n := entriesLen - cap(mi.entries); n > 0 {
		mi.entries = append(mi.entries[:cap(mi.entries)], make([]rollupResultCacheMetainfoEntry, n)...)
	}
	mi.entries = mi.entries[:entriesLen]
	for i := 0; i < entriesLen; i++ {
		tail, err := mi.entries[i].Unmarshal(src)
		if err != nil {
			return fmt.Errorf("cannot unmarshal entry #%d: %w", i, err)
		}
		src = tail
	}
	if len(src) > 0 {
		return fmt.Errorf("unexpected non-empty tail left; len(tail)=%d", len(src))
	}
	return nil
}

func (mi *rollupResultCacheMetainfo) CoversTimeRange(start, end int64) bool {
	if start > end {
		logger.Panicf("BUG: start cannot exceed end; got %d vs %d", start, end)
	}
	for i := range mi.entries {
		e := &mi.entries[i]
		if start >= e.start && end <= e.end {
			return true
		}
	}
	return false
}

func (mi *rollupResultCacheMetainfo) GetBestKey(start, end int64) rollupResultCacheKey {
	if start > end {
		logger.Panicf("BUG: start cannot exceed end; got %d vs %d", start, end)
	}
	var bestKey rollupResultCacheKey
	dMax := int64(0)
	for i := range mi.entries {
		e := &mi.entries[i]
		if start < e.start {
			continue
		}
		d := e.end - start
		if end <= e.end {
			d = end - start
		}
		if d >= dMax {
			dMax = d
			bestKey = e.key
		}
	}
	return bestKey
}

func (mi *rollupResultCacheMetainfo) AddKey(key rollupResultCacheKey, start, end int64) {
	if start > end {
		logger.Panicf("BUG: start cannot exceed end; got %d vs %d", start, end)
	}
	mi.entries = append(mi.entries, rollupResultCacheMetainfoEntry{
		start: start,
		end:   end,
		key:   key,
	})
	if len(mi.entries) > 30 {
		// Remove old entries.
		mi.entries = append(mi.entries[:0], mi.entries[10:]...)
	}
}

func (mi *rollupResultCacheMetainfo) RemoveKey(key rollupResultCacheKey) {
	for i := range mi.entries {
		if mi.entries[i].key == key {
			mi.entries = append(mi.entries[:i], mi.entries[i+1:]...)
			return
		}
	}
}

type rollupResultCacheMetainfoEntry struct {
	start int64
	end   int64
	key   rollupResultCacheKey
}

func (mie *rollupResultCacheMetainfoEntry) Marshal(dst []byte) []byte {
	dst = encoding.MarshalInt64(dst, mie.start)
	dst = encoding.MarshalInt64(dst, mie.end)
	dst = encoding.MarshalUint64(dst, mie.key.prefix)
	dst = encoding.MarshalUint64(dst, mie.key.suffix)
	return dst
}

func (mie *rollupResultCacheMetainfoEntry) Unmarshal(src []byte) ([]byte, error) {
	if len(src) < 8 {
		return src, fmt.Errorf("cannot unmarshal start from %d bytes; need at least %d bytes", len(src), 8)
	}
	mie.start = encoding.UnmarshalInt64(src)
	src = src[8:]

	if len(src) < 8 {
		return src, fmt.Errorf("cannot unmarshal end from %d bytes; need at least %d bytes", len(src), 8)
	}
	mie.end = encoding.UnmarshalInt64(src)
	src = src[8:]

	if len(src) < 8 {
		return src, fmt.Errorf("cannot unmarshal key prefix from %d bytes; need at least %d bytes", len(src), 8)
	}
	mie.key.prefix = encoding.UnmarshalUint64(src)
	src = src[8:]

	if len(src) < 8 {
		return src, fmt.Errorf("cannot unmarshal key suffix from %d bytes; need at least %d bytes", len(src), 8)
	}
	mie.key.suffix = encoding.UnmarshalUint64(src)
	src = src[8:]

	return src, nil
}

// rollupResultCacheKey must be globally unique across vmselect nodes,
// so it has prefix and suffix.
type rollupResultCacheKey struct {
	prefix uint64
	suffix uint64
}

func (k *rollupResultCacheKey) Marshal(dst []byte) []byte {
	dst = append(dst, rollupResultCacheVersion)
	dst = encoding.MarshalUint64(dst, k.prefix)
	dst = encoding.MarshalUint64(dst, k.suffix)
	return dst
}
