package admin

import (
	"math"
	runtimeMetrics "runtime/metrics"
)

func daemonMemoryMB() float64 {
	samples := []runtimeMetrics.Sample{
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
	}
	runtimeMetrics.Read(samples)
	total := samples[0].Value.Uint64()
	released := samples[1].Value.Uint64()
	if released > total {
		return 0
	}

	return math.Round(float64(total-released)/(1024*1024)*10) / 10
}
