package limiter

import (
	"sync"
	"time"
)

import (
	"go.uber.org/atomic"
)

var (
	_ Limiter = (*AutoConcurrency)(nil)
	_ Updater = (*AutoConcurrencyUpdater)(nil)
)

type AutoConcurrency struct {
	sync.RWMutex
	alpha          float64 //explore ratio TODO: it should be adjusted heuristically
	emaFactor      float64
	noLoadLatency  float64 //duration
	maxQPS         float64
	maxConcurrency uint64

	// metrics of the current round
	avgLatency slidingWindow

	inflight atomic.Uint64
}

func NewAutoConcurrencyLimiter() *AutoConcurrency {
	return &AutoConcurrency{
		alpha:          0,
		emaFactor:      0,
		noLoadLatency:  0,
		maxQPS:         0,
		maxConcurrency: 0,
		avgLatency:     slidingWindow{},
		inflight:       atomic.Uint64{},
	}
}
func (l *AutoConcurrency) updateNoLoadLatency(latency float64) {
	if l.noLoadLatency <= 0 {
		l.noLoadLatency = latency
	} else if latency < l.noLoadLatency {
		l.noLoadLatency = latency*l.emaFactor + l.noLoadLatency*(1-l.emaFactor)
	}
}

func (l *AutoConcurrency) updateQPS(qps float64) {
	if l.maxQPS <= qps {
		l.maxQPS = qps
	} else {
		l.maxQPS = qps*l.emaFactor + l.maxQPS*(1-l.emaFactor)
	}
}

func (l *AutoConcurrency) updateMaxConcurrency(v uint64) {
	if l.maxConcurrency <= v {
		l.maxConcurrency = v
	} else {
		l.maxConcurrency = uint64(float64(v)*l.emaFactor + float64(l.maxConcurrency)*(1-l.emaFactor))
	}
}

func (l *AutoConcurrency) Inflight() uint64 {
	return l.inflight.Load()
}

func (l *AutoConcurrency) Remaining() uint64 {
	return l.maxConcurrency - l.inflight.Load()
}

func (l *AutoConcurrency) Acquire() (Updater, error) {
	if l.inflight.Inc() > l.maxConcurrency {
		l.inflight.Dec()
		return nil, ErrReachLimitation
	}
	u := &AutoConcurrencyUpdater{
		startTime: time.Now(),
		limiter:   l,
	}
	return u, nil
}

type AutoConcurrencyUpdater struct {
	startTime time.Time
	limiter   *AutoConcurrency
}

func (u *AutoConcurrencyUpdater) DoUpdate() error {
	defer func() {
		u.limiter.inflight.Dec()
	}()
	latency := float64(time.Now().UnixMilli() - u.startTime.UnixMilli())
	u.limiter.avgLatency.Add(latency)
	u.limiter.Lock()
	defer u.limiter.Unlock()
	qpms := float64(u.limiter.avgLatency.Value()) * 1000000 / float64(u.limiter.avgLatency.TimespanNs())
	u.limiter.updateNoLoadLatency(latency)
	u.limiter.updateQPS(qpms)
	nextMaxConcurrency := u.limiter.maxQPS * ((2+u.limiter.alpha)*u.limiter.noLoadLatency - u.limiter.avgLatency.Avg())
	u.limiter.updateMaxConcurrency(uint64(nextMaxConcurrency))
	return nil
}

// slidingWindow is a policy for ring window based on time duration.
// slidingWindow moves bucket offset with time duration.
// e.g. If the last point is appended one bucket duration ago,
// slidingWindow will increment current offset.
type slidingWindow struct {
	size           int
	mu             sync.Mutex
	buckets        []AvgBucket //sum+cnt
	count          int64
	avg            float64
	sum            float64
	offset         int
	bucketDuration time.Duration
	lastAppendTime time.Time
}

type AvgBucket struct {
	cnt int64
	sum float64
}

// SlidingWindowAvgOpts contains the arguments for creating SlidingWindowCounter.
type SlidingWindowAvgOpts struct {
	Size           int
	BucketDuration time.Duration
}

// NewSlidingWindowAvg creates a new SlidingWindowCounter based on the given window and SlidingWindowCounterOpts.
func NewSlidingWindowAvg(opts SlidingWindowAvgOpts) *slidingWindow {
	buckets := make([]AvgBucket, opts.Size)

	return &slidingWindow{
		size:           opts.Size,
		offset:         0,
		buckets:        buckets,
		bucketDuration: opts.BucketDuration,
		lastAppendTime: time.Now(),
	}
}

func (c *slidingWindow) timespan() int {
	v := int(time.Since(c.lastAppendTime) / c.bucketDuration)
	if v > -1 { // maybe time backwards
		return v
	}
	return c.size
}

func (c *slidingWindow) Add(v float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	//move offset
	timespan := c.timespan()
	if timespan > 0 {
		start := (c.offset + 1) % c.size
		end := (c.offset + timespan) % c.size
		// reset the expired buckets
		c.ResetBuckets(start, timespan)
		c.offset = end
		c.lastAppendTime = c.lastAppendTime.Add(time.Duration(timespan * int(c.bucketDuration)))
	}

	c.buckets[c.offset].cnt++
	c.buckets[c.offset].sum += v

	c.sum += v
	c.count++
	c.avg = c.sum / float64(c.count)
}

func (c *slidingWindow) Value() float64 {
	return c.avg
}

func (c *slidingWindow) Count() int64 {
	return c.count
}

func (c *slidingWindow) Avg() float64 {
	return c.avg
}

func (c *slidingWindow) TimespanNs() int64 {
	return c.bucketDuration.Nanoseconds() * int64(c.size)
}

// ResetBucket empties the bucket based on the given offset.
func (c *slidingWindow) ResetBucket(offset int) {
	c.sum -= c.buckets[offset%c.size].sum
	c.count -= c.buckets[offset%c.size].cnt
	c.avg = c.sum / float64(c.count)

	c.buckets[offset%c.size].cnt = 0
	c.buckets[offset%c.size].sum = 0
}

// ResetBuckets empties the buckets based on the given offsets.
func (c *slidingWindow) ResetBuckets(offset int, count int) {
	if count > c.size {
		count = c.size
	}
	for i := 0; i < count; i++ {
		c.ResetBucket(offset + i)
	}
}
