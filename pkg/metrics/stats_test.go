package metrics

import (
	"math"
	"testing"
	"time"
)

func TestQuantileType7(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		q    float64
		want float64
	}{
		{0.00, 1},
		{0.50, 5.5},
		{0.90, 9.1},
		{1.00, 10},
	}
	for _, c := range cases {
		if got := quantile(v, c.q); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("quantile(%v) = %v, want %v", c.q, got, c.want)
		}
	}
}

func TestSummarizeSmallSampleHasNoInterval(t *testing.T) {
	s := NewSeries("create", "ms")
	for i := 0; i < bootstrapMinN-1; i++ {
		s.Add(float64(i))
	}
	got := s.Summarize()
	if got.CI95Lo != 0 || got.CI95Hi != 0 {
		t.Fatalf("interval reported for n=%d: [%v,%v]", got.N, got.CI95Lo, got.CI95Hi)
	}
}

func TestSummarizeIntervalBracketsMean(t *testing.T) {
	s := NewSeries("create", "ms")
	for i := 1; i <= 100; i++ {
		s.Add(float64(i))
	}
	got := s.Summarize()
	if got.N != 100 {
		t.Fatalf("N = %d, want 100", got.N)
	}
	if !(got.CI95Lo < got.Mean && got.Mean < got.CI95Hi) {
		t.Fatalf("mean %v outside interval [%v,%v]", got.Mean, got.CI95Lo, got.CI95Hi)
	}
}

func TestSummarizeIsDeterministic(t *testing.T) {
	build := func() Summary {
		s := NewSeries("x", "ms")
		for i := 1; i <= 50; i++ {
			s.Add(float64(i * i % 37))
		}
		return s.Summarize()
	}
	a, b := build(), build()
	if a != b {
		t.Fatalf("summary not reproducible:\n%+v\n%+v", a, b)
	}
}

func TestSummarizeDoesNotReorderSamples(t *testing.T) {
	s := NewSeries("x", "ms")
	s.Add(3)
	s.Add(1)
	s.Add(2)
	_ = s.Summarize()
	if s.Samples[0] != 3 || s.Samples[2] != 2 {
		t.Fatalf("Summarize mutated the series: %v", s.Samples)
	}
}

func TestAddDurationUsesMilliseconds(t *testing.T) {
	s := NewSeries("x", "ms")
	s.AddDuration(1500 * time.Microsecond)
	if math.Abs(s.Samples[0]-1.5) > 1e-9 {
		t.Fatalf("got %v ms, want 1.5", s.Samples[0])
	}
}

func TestThroughputZeroElapsed(t *testing.T) {
	if got := (Throughput{Count: 10}).PerSecond(); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestSlowdown(t *testing.T) {
	if got := Slowdown(100, 150); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("got %v, want 0.5", got)
	}
	if got := Slowdown(0, 150); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}
