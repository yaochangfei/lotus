package main

import (
	"math"
	"math/rand"
	"testing"
)

func TestStats(t *testing.T) {
	N := 16
	ss := make([]*stats, N)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < N; i++ {
		ss[i] = &stats{}
		maxJ := rng.Intn(1000)
		for j := 0; j < maxJ; j++ {
			ss[i].AddPoint(rng.NormFloat64()*5 + 500)
		}
		t.Logf("mean: %f, stddev: %f, count %f", ss[i].mean, math.Sqrt(ss[i].variance()), ss[i].count)
	}
	out := &stats{}
	for i := 0; i < N; i++ {
		out.Combine(ss[i])
		t.Logf("combine: mean: %f, stddev: %f", out.mean, math.Sqrt(out.variance()))
	}
}
