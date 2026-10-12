package harnesssdk

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Metrics are the numbers a turn's harness reported, such as tokens and
// cost, by name (see the guide's metrics section for the usual names). A
// Metrics value is immutable and comparable: two are equal when they hold
// the same numbers, so Result and Turn stay comparable. The zero value
// holds none.
type Metrics struct {
	enc string // "name=value\n" lines, sorted by name
}

func metricsOf(m map[string]float64) Metrics {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if k == "" || strings.ContainsAny(k, "=\n") {
			continue // not a metric name a definition can declare
		}
		v := m[k]
		if v == 0 {
			v = 0 // -0 and 0 are equal numbers: write them alike
		}
		b.WriteString(k + "=" + strconv.FormatFloat(v, 'g', -1, 64) + "\n")
	}
	return Metrics{enc: b.String()}
}

// Get returns one metric.
func (m Metrics) Get(name string) (float64, bool) {
	for line := range strings.SplitSeq(m.enc, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == name {
			f, err := strconv.ParseFloat(v, 64)
			return f, err == nil
		}
	}
	return 0, false
}

// Map returns a copy of every metric.
func (m Metrics) Map() map[string]float64 {
	out := map[string]float64{}
	for line := range strings.SplitSeq(m.enc, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				out[k] = f
			}
		}
	}
	return out
}

// String lists the metrics as name=value, sorted by name.
func (m Metrics) String() string {
	return strings.ReplaceAll(strings.TrimSuffix(m.enc, "\n"), "\n", " ")
}

// MarshalJSON writes the metrics as a JSON object.
func (m Metrics) MarshalJSON() ([]byte, error) { return json.Marshal(m.Map()) }

// UnmarshalJSON reads a JSON object of numbers.
func (m *Metrics) UnmarshalJSON(data []byte) error {
	var v map[string]float64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*m = metricsOf(v)
	return nil
}
