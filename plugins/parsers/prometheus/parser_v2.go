package prometheus

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"time"

	"github.com/matttproud/golang_protobuf_extensions/pbutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/metric"
)

func (p *Parser) parseV2(buf []byte) ([]telegraf.Metric, error) {
	var parser expfmt.TextParser
	var metrics []telegraf.Metric
	var err error

	// Make sure we have a finishing newline but no trailing one
	buf = bytes.TrimPrefix(buf, []byte("\n"))
	if !bytes.HasSuffix(buf, []byte("\n")) {
		buf = append(buf, []byte("\n")...)
	}

	// Read raw data
	buffer := bytes.NewBuffer(buf)
	reader := bufio.NewReader(buffer)

	// Prepare output
	metricFamilies := make(map[string]*dto.MetricFamily)
	mediatype, params, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
	if err == nil && mediatype == "application/vnd.google.protobuf" &&
		params["encoding"] == "delimited" &&
		params["proto"] == "io.prometheus.client.MetricFamily" {
		for {
			mf := &dto.MetricFamily{}
			if _, ierr := pbutil.ReadDelimited(reader, mf); ierr != nil {
				if errors.Is(ierr, io.EOF) {
					break
				}
				return nil, fmt.Errorf("reading metric family protocol buffer failed: %w", ierr)
			}
			metricFamilies[mf.GetName()] = mf
		}
	} else {
		metricFamilies, err = parser.TextToMetricFamilies(reader)
		if err != nil {
			return nil, fmt.Errorf("reading text format failed: %w", err)
		}
	}

	now := time.Now()

	// read metrics
	for metricName, mf := range metricFamilies {
		for _, m := range mf.Metric {
			// reading tags
			tags := GetTagsFromLabels(m, p.DefaultTags)
			t := p.getTimestampV2(m, now)

			if mf.GetType() == dto.MetricType_SUMMARY {
				// summary metric
				telegrafMetrics := makeQuantilesV2(m, tags, metricName, mf.GetType(), t)
				metrics = append(metrics, telegrafMetrics...)
			} else if mf.GetType() == dto.MetricType_HISTOGRAM {
				// histogram metric
				telegrafMetrics := makeBucketsV2(m, tags, metricName, mf.GetType(), t)
				metrics = append(metrics, telegrafMetrics...)
			} else {
				// standard metric
				// reading fields
				fields := getNameAndValueV2(m, metricName)
				// converting to telegraf metric
				if len(fields) > 0 {
					m := metric.New("prometheus", tags, fields, t, ValueType(mf.GetType()))
					metrics = append(metrics, m)
				}
			}
		}
	}

	return metrics, err
}

func (p *Parser) ParseLine(line string) (telegraf.Metric, error) {
	metrics, err := p.Parse([]byte(line))
	if err != nil {
		return nil, err
	}

	if len(metrics) < 1 {
		return nil, fmt.Errorf("no metrics in line")
	}

	if len(metrics) > 1 {
		return nil, fmt.Errorf("more than one metric in line")
	}

	return metrics[0], nil
}

// Get Quantiles for summary metric & Buckets for histogram
func makeQuantilesV2(m *dto.Metric, tags map[string]string, metricName string, metricType dto.MetricType, t time.Time) []telegraf.Metric {
	metrics := make([]telegraf.Metric, 0, len(m.GetSummary().Quantile)+1)
	fields := make(map[string]interface{})

	fields[metricName+"_count"] = float64(m.GetSummary().GetSampleCount())
	fields[metricName+"_sum"] = m.GetSummary().GetSampleSum()
	met := metric.New("prometheus", tags, fields, t, ValueType(metricType))
	metrics = append(metrics, met)

	for _, q := range m.GetSummary().Quantile {
		newTags := tags
		fields = make(map[string]interface{})

		newTags["quantile"] = fmt.Sprint(q.GetQuantile())
		fields[metricName] = q.GetValue()

		quantileMetric := metric.New("prometheus", newTags, fields, t, ValueType(metricType))
		metrics = append(metrics, quantileMetric)
	}
	return metrics
}

// Get Buckets  from histogram metric
func makeBucketsV2(m *dto.Metric, tags map[string]string, metricName string, metricType dto.MetricType, t time.Time) []telegraf.Metric {
	metrics := make([]telegraf.Metric, 0, len(m.GetHistogram().Bucket)+2)
	fields := make(map[string]interface{})

	fields[metricName+"_count"] = float64(m.GetHistogram().GetSampleCount())
	fields[metricName+"_sum"] = m.GetHistogram().GetSampleSum()

	met := metric.New("prometheus", tags, fields, t, ValueType(metricType))
	metrics = append(metrics, met)

	infSeen := false
	for _, b := range m.GetHistogram().Bucket {
		newTags := tags
		fields = make(map[string]interface{})
		newTags["le"] = fmt.Sprint(b.GetUpperBound())
		fields[metricName+"_bucket"] = float64(b.GetCumulativeCount())

		histogramMetric := metric.New("prometheus", newTags, fields, t, ValueType(metricType))
		metrics = append(metrics, histogramMetric)
		if math.IsInf(b.GetUpperBound(), +1) {
			infSeen = true
		}
	}
	// Infinity bucket is required for proper function of histogram in prometheus
	if !infSeen {
		newTags := tags
		newTags["le"] = "+Inf"

		fields = make(map[string]interface{})
		fields[metricName+"_bucket"] = float64(m.GetHistogram().GetSampleCount())

		histogramInfMetric := metric.New("prometheus", newTags, fields, t, ValueType(metricType))
		metrics = append(metrics, histogramInfMetric)
	}
	return metrics
}

// Get name and value from metric
func getNameAndValueV2(m *dto.Metric, metricName string) map[string]interface{} {
	fields := make(map[string]interface{})
	if m.Gauge != nil {
		if !math.IsNaN(m.GetGauge().GetValue()) {
			fields[metricName] = m.GetGauge().GetValue()
		}
	} else if m.Counter != nil {
		if !math.IsNaN(m.GetCounter().GetValue()) {
			fields[metricName] = m.GetCounter().GetValue()
		}
	} else if m.Untyped != nil {
		if !math.IsNaN(m.GetUntyped().GetValue()) {
			fields[metricName] = m.GetUntyped().GetValue()
		}
	}
	return fields
}

func (p *Parser) getTimestampV2(m *dto.Metric, now time.Time) time.Time {
	var t time.Time
	if !p.IgnoreTimestamp && m.TimestampMs != nil && *m.TimestampMs > 0 {
		t = time.Unix(0, m.GetTimestampMs()*1000000)
	} else {
		t = now
	}
	return t
}
