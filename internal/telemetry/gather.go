package telemetry

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// ZeroCounterFilter wraps a prometheus.Gatherer and drops counter metrics
// whose value is 0. A counter at 0 means "this event never occurred" and
// carries no information — omitting it reduces scrape size without loss.
// Gauges, histograms, summaries, and untyped metrics are always passed through.
type ZeroCounterFilter struct {
	Inner prometheus.Gatherer
}

// Gather collects metric families from the inner gatherer and removes any
// counter-typed metric with value 0. Families that become empty after
// filtering are dropped entirely.
func (f *ZeroCounterFilter) Gather() ([]*dto.MetricFamily, error) {
	families, err := f.Inner.Gather()
	if err != nil {
		return families, fmt.Errorf("gathering metrics: %w", err)
	}

	result := make([]*dto.MetricFamily, 0, len(families))
	for _, fam := range families {
		if fam.GetType() != dto.MetricType_COUNTER {
			result = append(result, fam)

			continue
		}

		nonZero := make([]*dto.Metric, 0, len(fam.GetMetric()))
		for _, m := range fam.GetMetric() {
			if m.GetCounter().GetValue() != 0 {
				nonZero = append(nonZero, m)
			}
		}

		if len(nonZero) > 0 {
			fam.Metric = nonZero
			result = append(result, fam)
		}
	}

	return result, nil
}
