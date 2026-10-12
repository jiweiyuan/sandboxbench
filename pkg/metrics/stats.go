// Package metrics turns raw latency samples into the summary a Systems paper
// can defend: order statistics rather than a mean, and an interval rather than
// a point.
package metrics

import (
	"math"
	"math/rand"
	"sort"
	"time"
)

// Series accumulates samples of one measured quantity.
//
// A Series is not safe for concurrent use. Cluster workloads collect samples
// per goroutine and merge them under their own lock, which also keeps the
// sample order deterministic.
type Series struct {
	Name    string    `json:"name"`
	Unit    string    `json:"unit"`
	Samples []float64 `json:"samples"`
}

// NewSeries starts a series. Unit is carried into the report so a reader never
// has to guess whether a number is milliseconds or seconds.
func NewSeries(name, unit string) *Series {
	return &Series{Name: name, Unit: unit}
}

// Add records one sample.
func (s *Series) Add(v float64) { s.Samples = append(s.Samples, v) }

// AddDuration records one duration as milliseconds.
func (s *Series) AddDuration(d time.Duration) {
	s.Add(float64(d) / float64(time.Millisecond))
}

// Len reports the sample count.
func (s *Series) Len() int { return len(s.Samples) }

// Summary is the reported form of a Series.
type Summary struct {
	Name   string  `json:"name"`
	Unit   string  `json:"unit"`
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
	Stddev float64 `json:"stddev"`
	P50    float64 `json:"p50"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	// CI95Lo and CI95Hi bound the mean by a bootstrap percentile interval.
	// They are zero when N is below bootstrapMinN.
	CI95Lo float64 `json:"ci95_lo"`
	CI95Hi float64 `json:"ci95_hi"`
}

// bootstrapMinN is the sample count below which an interval would be theatre
// rather than evidence.
const bootstrapMinN = 10

const bootstrapResamples = 2000

// Summarize computes the summary. It copies the samples before sorting, so a
// caller may keep adding to the Series afterwards.
func (s *Series) Summarize() Summary {
	out := Summary{Name: s.Name, Unit: s.Unit, N: len(s.Samples)}
	if out.N == 0 {
		return out
	}
	v := make([]float64, len(s.Samples))
	copy(v, s.Samples)
	sort.Float64s(v)

	out.Min, out.Max = v[0], v[len(v)-1]
	out.Mean = mean(v)
	out.Stddev = stddev(v, out.Mean)
	out.P50 = quantile(v, 0.50)
	out.P90 = quantile(v, 0.90)
	out.P95 = quantile(v, 0.95)
	out.P99 = quantile(v, 0.99)

	if out.N >= bootstrapMinN {
		out.CI95Lo, out.CI95Hi = bootstrapMeanCI(v, bootstrapResamples, 0.95)
	}
	return out
}

func mean(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func stddev(v []float64, m float64) float64 {
	if len(v) < 2 {
		return 0
	}
	var ss float64
	for _, x := range v {
		d := x - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(v)-1))
}

// quantile uses the linear-interpolation definition (type 7, the R default) on
// an already sorted slice.
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// bootstrapMeanCI returns a percentile bootstrap interval for the mean. The
// generator is seeded from the sample values, so a given sample set always
// yields the same interval and a reported number can be recomputed.
func bootstrapMeanCI(v []float64, resamples int, level float64) (lo, hi float64) {
	n := len(v)
	var seed int64
	for i, x := range v {
		seed += int64(math.Float64bits(x)) + int64(i)
	}
	rng := rand.New(rand.NewSource(seed))

	means := make([]float64, resamples)
	for i := range means {
		var sum float64
		for j := 0; j < n; j++ {
			sum += v[rng.Intn(n)]
		}
		means[i] = sum / float64(n)
	}
	sort.Float64s(means)
	alpha := (1 - level) / 2
	return quantile(means, alpha), quantile(means, 1-alpha)
}

// Throughput is a rate derived from a count and an elapsed time.
type Throughput struct {
	Count   int           `json:"count"`
	Elapsed time.Duration `json:"elapsed_ns"`
}

// PerSecond reports the rate. It returns 0 for a zero elapsed time rather
// than an infinity that would poison a report.
func (t Throughput) PerSecond() float64 {
	if t.Elapsed <= 0 {
		return 0
	}
	return float64(t.Count) / t.Elapsed.Seconds()
}

// Slowdown is the contention metric from the noisy-neighbour experiment:
// how much longer the same work takes once interference runs beside it.
//
//	slowdown = contended/baseline - 1
func Slowdown(baseline, contended float64) float64 {
	if baseline <= 0 {
		return 0
	}
	return contended/baseline - 1
}
