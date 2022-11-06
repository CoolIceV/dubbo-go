package limiter

import (
	"dubbo.apache.org/dubbo-go/v3/filter/adaptivesvc/limiter/cpu"
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
	_       Limiter        = (*AutoConcurrency)(nil)
	_       Updater        = (*AutoConcurrencyUpdater)(nil)
	cpuLoad *atomic.Uint64 = atomic.NewUint64(0) // 0-1000
)

const (
	MaxExploreRatio    = 0.3
	MinExploreRatio    = 0.06
	SampleWindowSizeMs = 1000
	MinSampleCount     = 40
	MaxSampleCount     = 500
	CpuDecay           = 0.95
)

type AutoConcurrency struct {
	sync.RWMutex

	ExploreRatio   float64
	emaFactor      float64
	noLoadLatency  float64 //duration
	maxQPS         float64
	HalfIntervalMS int64
	maxConcurrency uint64

	// metrics of the current round
	StartSampleTimeUs  int64
	LastSamplingTimeUs *atomic.Int64
	ResetLatencyUs     int64 // time to reset noLoadLatency
	RemeasureStartUs   int64 //time to reset req data (SampleCount, TotalSampleUs, TotalReqCount)
	SampleCount        int64
	TotalSampleUs      int64
	TotalReqCount      *atomic.Int64

	prevDropTime *atomic.Duration

	inflight *atomic.Uint64
}

func init() {
	go cpuproc()
}

// cpu = cpuᵗ⁻¹ * decay + cpuᵗ * (1 - decay)
func cpuproc() {
	ticker := time.NewTicker(time.Millisecond * 500) // same to cpu sample rate
	defer func() {
		ticker.Stop()
		if err := recover(); err != nil {
			go cpuproc()
		}
	}()

	for range ticker.C {
		usage := cpu.CpuUsage()
		prevCPU := cpuLoad.Load()
		curCPU := uint64(float64(prevCPU)*CpuDecay + float64(usage)*(1.0-CpuDecay))
		logger.Debugf("current cpu usage: %d", curCPU)
		cpuLoad.Store(curCPU)
	}
}

func CpuUsage() uint64 {
	return cpuLoad.Load()
}

func NewAutoConcurrencyLimiter() *AutoConcurrency {
	l := &AutoConcurrency{
		ExploreRatio:       MaxExploreRatio,
		emaFactor:          0.1,
		noLoadLatency:      -1,
		maxQPS:             -1,
		maxConcurrency:     20,
		HalfIntervalMS:     25000,
		ResetLatencyUs:     0,
		inflight:           atomic.NewUint64(0),
		LastSamplingTimeUs: atomic.NewInt64(0),
		TotalReqCount:      atomic.NewInt64(0),
		prevDropTime:       atomic.NewDuration(0),
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
		prevDrop := l.prevDropTime.Load()
		nowDuration := time.Duration(now.Unix())
		if CpuUsage() >= 500 || nowDuration-prevDrop <= time.Second { // only when cpu load is above 50% or less than 1s since last drop
			l.inflight.Dec()
			l.prevDropTime.CAS(prevDrop, nowDuration)
			return nil, ErrReachLimitation
		}
	}

	u := &AutoConcurrencyUpdater{
		startTime: now,
		limiter:   l,
	}
	return u, nil
}

func (l *AutoConcurrency) Reset(startTimeUs int64) {
	l.StartSampleTimeUs = startTimeUs
	l.SampleCount = 0
	l.TotalSampleUs = 0
	l.TotalReqCount.Store(0)
}

func (l *AutoConcurrency) NextResetTime(samplingTimeUs int64) int64 {
	return samplingTimeUs + (l.HalfIntervalMS+rand.Int63n(l.HalfIntervalMS))*1000
}

func (l *AutoConcurrency) Update(latency int64, samplingTimeUs int64) {
	l.Lock()
	defer l.Unlock()
	if l.ResetLatencyUs != 0 { // wait to reset noLoadLatency and other data
		if l.ResetLatencyUs > samplingTimeUs {
			return
		}
		l.noLoadLatency = -1
		l.ResetLatencyUs = 0
		l.RemeasureStartUs = l.NextResetTime(samplingTimeUs)
		l.Reset(samplingTimeUs)
	}

	if l.StartSampleTimeUs == 0 {
		l.StartSampleTimeUs = samplingTimeUs
	}

	l.SampleCount++
	l.TotalSampleUs += latency

	logger.Debugf("[Auto Concurrency Limiter Test] samplingTimeUs: %v, StartSampleTimeUs: %v", samplingTimeUs, l.StartSampleTimeUs)

	if l.SampleCount < MinSampleCount {
		if samplingTimeUs-l.StartSampleTimeUs >= SampleWindowSizeMs*1000 { // QPS is too small
			l.Reset(samplingTimeUs)
		}
		return
	}

	logger.Debugf("[Auto Concurrency Limiter Test] samplingTimeUs: %v, StartSampleTimeUs: %v", samplingTimeUs, l.StartSampleTimeUs)

	// sampling time is too short. If sample count is bigger than MaxSampleCount, just update.
	if samplingTimeUs-l.StartSampleTimeUs < SampleWindowSizeMs*1000 && l.SampleCount < MaxSampleCount {
		return
	}

	if l.SampleCount > 0 {
		qps := l.TotalReqCount.Load() / (samplingTimeUs - l.StartSampleTimeUs) * 1000000.0
		l.updateQPS(float64(qps))

		avgLatency := l.TotalSampleUs / l.SampleCount
		l.updateNoLoadLatency(float64(avgLatency))

		nextMaxConcurrency := uint64(0)
		if l.RemeasureStartUs <= samplingTimeUs { // should reset
			l.Reset(samplingTimeUs)
			l.ResetLatencyUs = samplingTimeUs + avgLatency*2
			nextMaxConcurrency = uint64(math.Ceil(l.maxQPS * l.noLoadLatency * 0.9 / 1000000))
		} else {
			// use explore ratio to adjust MaxConcurrency. [Conditions may need to be reconsidered] !!!
			if float64(avgLatency) <= l.noLoadLatency*(1.0+MinExploreRatio) ||
				float64(qps) >= l.maxQPS*(1.0+MinExploreRatio) {
				l.ExploreRatio = math.Min(MaxExploreRatio, l.ExploreRatio+0.02)
			} else {
				l.ExploreRatio = math.Max(MinExploreRatio, l.ExploreRatio-0.02)
			}
			nextMaxConcurrency = uint64(math.Ceil(l.noLoadLatency * l.maxQPS * (1 + l.ExploreRatio) / 1000000))
		}
		l.maxConcurrency = nextMaxConcurrency
	} else {
		// There may be no more data because the service is overloaded, reducing concurrency and maxConcurrency should be no less than 1
		l.maxConcurrency /= 2
		if l.maxConcurrency <= 0 {
			l.maxConcurrency = 1
		}
	}

	logger.Debugf("[Auto Concurrency Limiter] Qps: %v, NoLoadLatency: %f, MaxConcurrency: %d, limiter: %+v",
		l.maxQPS, l.noLoadLatency, l.maxConcurrency, l)

	// Update completed, resample
	l.Reset(samplingTimeUs)

}

type AutoConcurrencyUpdater struct {
	startTime time.Time
	limiter   *AutoConcurrency
}

func (u *AutoConcurrencyUpdater) DoUpdate() error {
	defer func() {
		u.limiter.inflight.Dec()
	}()
	u.limiter.TotalReqCount.Add(1)
	now := time.Now().UnixNano() / 1e3
	lastSamplingTimeUs := u.limiter.LastSamplingTimeUs.Load()
	if lastSamplingTimeUs == 0 || now-lastSamplingTimeUs >= 100 {
		sample := u.limiter.LastSamplingTimeUs.CAS(lastSamplingTimeUs, now)
		if sample {
			logger.Debugf("[Auto Concurrency Updater] sample, %v, %v", u.limiter.ResetLatencyUs, u.limiter.RemeasureStartUs)
			latency := now - u.startTime.UnixNano()/1e3
			u.limiter.Update(latency, now)
		}
	}

	return nil
}
