package limiter

import (
	"github.com/dubbogo/gost/log/logger"
	"math"
	"math/rand"
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

const (
	MaxExploreRatio    = 0.3
	MinExploreRatio    = 0.06
	SampleWindowSizeMs = 1000
	MinSampleCount     = 40
	MaxSampleCount     = 200
	FailPunishRatio    = 1.0
)

type AutoConcurrency struct {
	sync.RWMutex

	ExploreRatio   float64
	emaFactor      float64
	noLoadLatency  float64 //duration
	maxQPS         float64
	HalfIntervalMS int64
	maxConcurrency uint64
	//CorrectionFactor float64
	// metrics of the current round
	StartTimeUs        int64
	LastSamplingTimeUs *atomic.Int64
	ResetLatencyUs     int64
	RemeasureStartUs   int64

	SuccessCount   int64
	FailCount      int64
	TotalSuccessUs int64
	TotalFailUs    int64
	TotalSuccReq   *atomic.Int64

	inflight *atomic.Uint64
}

func NewAutoConcurrencyLimiter() *AutoConcurrency {
	l := &AutoConcurrency{
		ExploreRatio:       MaxExploreRatio,
		emaFactor:          0.1,
		noLoadLatency:      -1,
		maxQPS:             -1,
		maxConcurrency:     40,
		HalfIntervalMS:     25000,
		ResetLatencyUs:     0,
		inflight:           atomic.NewUint64(0),
		LastSamplingTimeUs: atomic.NewInt64(0),
		TotalSuccReq:       atomic.NewInt64(0),
	}
	l.RemeasureStartUs = l.NextResetTime(time.Now().UnixNano() / 1e3)
	return l
}

func (l *AutoConcurrency) updateNoLoadLatency(latency float64) {
	emaFactor := l.emaFactor
	if l.noLoadLatency <= 0 {
		l.noLoadLatency = latency
	} else if latency < l.noLoadLatency {
		l.noLoadLatency = latency*emaFactor + l.noLoadLatency*(1-emaFactor)
	}
}

func (l *AutoConcurrency) updateQPS(qps float64) {
	emaFactor := l.emaFactor / 10
	if l.maxQPS <= qps {
		l.maxQPS = qps
	} else {
		l.maxQPS = qps*emaFactor + l.maxQPS*(1-emaFactor)
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
	now := time.Now()
	if l.inflight.Inc() > l.maxConcurrency {
		l.inflight.Dec()
		return nil, ErrReachLimitation
	}

	u := &AutoConcurrencyUpdater{
		startTime: now,
		limiter:   l,
	}
	return u, nil
}

func (l *AutoConcurrency) Reset(startTimeUs int64) {
	l.StartTimeUs = startTimeUs
	l.SuccessCount = 0
	l.FailCount = 0
	l.TotalFailUs = 0
	l.TotalSuccessUs = 0
	l.TotalSuccReq.Store(0)
}

func (l *AutoConcurrency) NextResetTime(samplingTimeUs int64) int64 {
	return samplingTimeUs + (l.HalfIntervalMS+rand.Int63n(l.HalfIntervalMS))*1000
}

func (l *AutoConcurrency) Update(err error, latency int64, samplingTimeUs int64) {
	l.Lock()
	defer l.Unlock()
	if l.ResetLatencyUs != 0 {
		if l.ResetLatencyUs > samplingTimeUs {
			return
		}
		l.noLoadLatency = -1
		l.ResetLatencyUs = 0
		l.RemeasureStartUs = l.NextResetTime(samplingTimeUs)
		l.Reset(samplingTimeUs)
	}

	if l.StartTimeUs == 0 {
		l.StartTimeUs = samplingTimeUs
	}

	if err != nil {
		l.FailCount++
		l.TotalFailUs += latency
	} else {
		l.SuccessCount++
		l.TotalSuccessUs += latency
	}
	if l.SuccessCount+l.FailCount < MinSampleCount {
		if samplingTimeUs-l.StartTimeUs >= SampleWindowSizeMs*1000 {
			l.Reset(samplingTimeUs)
		}
		return
	}

	logger.Debugf("[Auto Concurrency Limiter Test] samplingTimeUs: %v, StartTimeUs: %v", samplingTimeUs, l.StartTimeUs)

	if samplingTimeUs-l.StartTimeUs < SampleWindowSizeMs*1000 && l.SuccessCount+l.FailCount < MaxSampleCount {
		return
	}

	if l.SuccessCount > 0 {
		totalSuccReq := l.TotalSuccReq.Load()
		avgLatency := (l.TotalFailUs*FailPunishRatio + l.TotalSuccessUs) / l.SuccessCount
		qps := 1000000.0 * totalSuccReq / (samplingTimeUs - l.StartTimeUs)
		l.updateQPS(float64(qps))
		l.updateNoLoadLatency(float64(avgLatency))
		logger.Debugf("[Auto Concurrency Limiter] success count: %v, fail count: %v, limiter: %+v", l.SuccessCount, l.FailCount, l)
		nextMaxConcurrency := uint64(0)
		if l.RemeasureStartUs <= samplingTimeUs {
			l.Reset(samplingTimeUs)
			l.ResetLatencyUs = samplingTimeUs + avgLatency*2
			nextMaxConcurrency = uint64(math.Ceil(l.maxQPS * l.noLoadLatency / 1000000 * 0.9))
		} else {
			//nextMaxConcurrency := u.limiter.maxQPS * ((1 + u.limiter.alpha) * u.limiter.noLoadLatency)
			if float64(avgLatency) <= l.noLoadLatency*(1.0+MinExploreRatio*1.0) ||
				float64(qps) <= l.maxQPS/(1.0+MinExploreRatio) {
				l.ExploreRatio = math.Min(MaxExploreRatio, l.ExploreRatio+0.02)
			} else {
				l.ExploreRatio = math.Max(MinExploreRatio, l.ExploreRatio-0.02)
			}
			nextMaxConcurrency = uint64(math.Ceil(l.noLoadLatency * l.maxQPS * (1 + l.ExploreRatio) / 1000000))
		}
		l.maxConcurrency = nextMaxConcurrency
	} else {
		l.maxConcurrency /= 2
	}
	l.Reset(samplingTimeUs)

	logger.Debugf("[Auto Concurrency Limiter] Qps: %v, NoLoadLatency: %f, MaxConcurrency: %d, limiter: %+v",
		l.maxQPS, l.noLoadLatency, l.maxConcurrency, l)
}

type AutoConcurrencyUpdater struct {
	startTime time.Time
	limiter   *AutoConcurrency
}

func (u *AutoConcurrencyUpdater) DoUpdate(err error) error {
	defer func() {
		u.limiter.inflight.Dec()
	}()
	if err == nil {
		u.limiter.TotalSuccReq.Add(1)
	}
	now := time.Now().UnixNano() / 1e3
	lastSamplingTimeUs := u.limiter.LastSamplingTimeUs.Load()
	if lastSamplingTimeUs == 0 || now-lastSamplingTimeUs >= 100 {
		sample := u.limiter.LastSamplingTimeUs.CAS(lastSamplingTimeUs, now)
		if sample {
			logger.Debugf("[Auto Concurrency Updater] sample, %v, %v", u.limiter.ResetLatencyUs, u.limiter.RemeasureStartUs)
			latency := now - u.startTime.UnixNano()/1e3
			u.limiter.Update(err, latency, now)
		}
	}

	return nil
}
