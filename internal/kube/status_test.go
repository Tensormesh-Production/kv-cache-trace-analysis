package kube

import "testing"

func TestParseCounts(t *testing.T) {
	text := `# HELP otelcol_exporter_sent_spans Number of spans successfully sent to destination.
# TYPE otelcol_exporter_sent_spans counter
otelcol_exporter_sent_log_records{exporter="file/request-trace"} 300
otelcol_exporter_sent_spans{exporter="file/request-trace"} 280
otelcol_exporter_sent_spans_total{exporter="file/request-trace",service_name="otelcol-contrib"} 20
otelcol_exporter_sent_spans{exporter="otlp/tempo"} 999
otelcol_processor_filter_logs_filtered{filter="filter/deadline"} 4
otelcol_processor_filter_spans_filtered{filter="filter/deadline"} 6
otelcol_processor_filter_spans_filtered{filter="filter/request-trace"} 50
otelcol_process_uptime 7.48
`
	c := ParseCounts(text)
	if c.Spans != 300 || c.LogRecords != 300 || c.AfterDeadline != 10 {
		t.Errorf("ParseCounts = %+v", c)
	}
	if c := ParseCounts(""); c.Spans != 0 || c.LogRecords != 0 {
		t.Errorf("empty: %+v", c)
	}
}
