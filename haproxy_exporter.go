package main

import (
	"encoding/csv"
	"flag"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "haproxy" // For Prometheus metrics.

	// HAProxy 1.4
	// # pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,weight,act,bck,chkfail,chkdown,lastchg,downtime,qlimit,pid,iid,sid,throttle,lbtot,tracked,type,rate,rate_lim,rate_max,check_status,check_code,check_duration,hrsp_1xx,hrsp_2xx,hrsp_3xx,hrsp_4xx,hrsp_5xx,hrsp_other,hanafail,req_rate,req_rate_max,req_tot,cli_abrt,srv_abrt,
	// HAProxy 1.5
	// pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,weight,act,bck,chkfail,chkdown,lastchg,downtime,qlimit,pid,iid,sid,throttle,lbtot,tracked,type,rate,rate_lim,rate_max,check_status,check_code,check_duration,hrsp_1xx,hrsp_2xx,hrsp_3xx,hrsp_4xx,hrsp_5xx,hrsp_other,hanafail,req_rate,req_rate_max,req_tot,cli_abrt,srv_abrt,comp_in,comp_out,comp_byp,comp_rsp,lastsess,
	expectedCsvFieldCount = 52
)

var (
	frontendLabelNames = []string{"frontend"}
	backendLabelNames  = []string{"backend"}
	serverLabelNames   = []string{"backend", "server"}
)

func newFrontendMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "frontend_" + metricName,
			Help:        docString,
			ConstLabels: constLabels,
		},
		frontendLabelNames,
	)
}

func newBackendMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "backend_" + metricName,
			Help:        docString,
			ConstLabels: constLabels,
		},
		backendLabelNames,
	)
}

func newServerMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "server_" + metricName,
			Help:        docString,
			ConstLabels: constLabels,
		},
		serverLabelNames,
	)
}

// Exporter collects HAProxy stats from the given URI and exports them using
// the prometheus metrics package.
type Exporter struct {
	URI   string
	mutex sync.RWMutex

	up                                             prometheus.Gauge
	totalScrapes, csvParseFailures                 prometheus.Counter
	frontendMetrics, backendMetrics, serverMetrics map[int]*prometheus.GaugeVec
}

// NewExporter returns an initialized Exporter.
func NewExporter(uri string, haProxyServerMetricFields string) *Exporter {
	return &Exporter{
		URI: uri,
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last scrape of haproxy successful.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_total_scrapes",
			Help:      "Current total HAProxy scrapes.",
		}),
		csvParseFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_csv_parse_failures",
			Help:      "Number of errors while parsing CSV.",
		}),
		backendMetrics: map[int]*prometheus.GaugeVec{
			2:  newBackendMetric("current_queue", "Current server queue length.", nil),
			3:  newBackendMetric("max_queue", "Maximum server queue length.", nil),
			4:  newBackendMetric("current_sessions", "Current number of active sessions.", nil),
			5:  newBackendMetric("max_sessions", "Maximum number of active sessions.", nil),
			7:  newBackendMetric("connections_total", "Total number of connections.", nil),
			8:  newBackendMetric("bytes_in_total", "Current total of incoming bytes.", nil),
			9:  newBackendMetric("bytes_out_total", "Current total of outgoing bytes.", nil),
			13: newBackendMetric("connection_errors_total", "Total of connection errors.", nil),
			14: newBackendMetric("response_errors_total", "Total of response errors.", nil),
			15: newBackendMetric("retry_warnings_total", "Total of retry warnings.", nil),
			16: newBackendMetric("redispatch_warnings_total", "Total of redispatch warnings.", nil),
			17: newBackendMetric("up", "Current health status of the backend (1 = UP, 0 = DOWN).", nil),
			33: newBackendMetric("current_session_rate", "Current number of sessions per second over last elapsed second.", nil),
			35: newBackendMetric("max_session_rate", "Maximum number of sessions per second.", nil),
			39: newBackendMetric("http_responses_total", "Total of HTTP responses.", prometheus.Labels{"code": "1xx"}),
			40: newBackendMetric("http_responses_total", "Total of HTTP responses.", prometheus.Labels{"code": "2xx"}),
			41: newBackendMetric("http_responses_total", "Total of HTTP responses.", prometheus.Labels{"code": "3xx"}),
			42: newBackendMetric("http_responses_total", "Total of HTTP responses.", prometheus.Labels{"code": "4xx"}),
			43: newBackendMetric("http_responses_total", "Total of HTTP responses.", prometheus.Labels{"code": "5xx"}),
			44: newBackendMetric("http_responses_total", "Total of HTTP responses.", prometheus.Labels{"code": "other"}),
		},
	}
}

// Describe describes all the metrics ever exported by the HAProxy exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range e.backendMetrics {
		m.Describe(ch)
	}
	ch <- e.up.Desc()
	ch <- e.totalScrapes.Desc()
	ch <- e.csvParseFailures.Desc()
}

// Collect fetches the stats from configured HAProxy location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	csvRows := make(chan []string)

	go e.scrape(csvRows)

	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()
	e.resetMetrics()
	e.setMetrics(csvRows)
	ch <- e.up
	ch <- e.totalScrapes
	ch <- e.csvParseFailures
	e.collectMetrics(ch)
}

func (e *Exporter) scrape(csvRows chan<- []string) {
	defer close(csvRows)

	e.totalScrapes.Inc()

	resp, err := http.Get(e.URI)
	if err != nil {
		e.up.Set(0)
		log.Printf("Error while scraping HAProxy: %v", err)
		return
	}
	defer resp.Body.Close()
	e.up.Set(1)

	reader := csv.NewReader(resp.Body)
	reader.TrailingComma = true
	reader.Comment = '#'

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Error while reading CSV: %v", err)
			e.csvParseFailures.Inc()
			break
		}
		if len(row) == 0 {
			continue
		}
		csvRows <- row
	}
}

func (e *Exporter) resetMetrics() {
	for _, m := range e.backendMetrics {
		m.Reset()
	}
}

func (e *Exporter) collectMetrics(metrics chan<- prometheus.Metric) {
	for _, m := range e.backendMetrics {
		m.Collect(metrics)
	}
}

func (e *Exporter) setMetrics(csvRows <-chan []string) {
	for csvRow := range csvRows {
		if len(csvRow) < expectedCsvFieldCount {
			log.Printf("Wrong CSV field count: %d vs. %d", len(csvRow), expectedCsvFieldCount)
			e.csvParseFailures.Inc()
			continue
		}

		pxname, _, type_ := csvRow[0], csvRow[1], csvRow[32]

		const (
			frontend = "0"
			backend  = "1"
			server   = "2"
			listener = "3"
		)

		switch type_ {
		case backend:
			e.exportCsvFields(e.backendMetrics, csvRow, pxname)
		}

	}
}

func (e *Exporter) exportCsvFields(metrics map[int]*prometheus.GaugeVec, csvRow []string, labels ...string) {
	for fieldIdx, metric := range metrics {
		valueStr := csvRow[fieldIdx]
		if valueStr == "" {
			continue
		}

		var value int64
		var err error
		switch valueStr {
		// UP or UP going down
		case "UP", "UP 1/3", "UP 2/3":
			value = 1
		// DOWN or DOWN going up
		case "DOWN", "DOWN 1/2":
			value = 0
		case "OPEN":
			value = 0
		case "no check":
			continue
		default:
			value, err = strconv.ParseInt(valueStr, 10, 64)
			if err != nil {
				log.Printf("Error while parsing CSV field value %s: %v", valueStr, err)
				e.csvParseFailures.Inc()
				continue
			}
		}
		metric.WithLabelValues(labels...).Set(float64(value))
	}
}

func main() {
	var (
		listeningAddress          = flag.String("telemetry.address", ":8080", "Address on which to expose metrics.")
		metricsEndpoint           = flag.String("telemetry.endpoint", "/metrics", "Path under which to expose metrics.")
		haProxyScrapeUri          = flag.String("haproxy.scrape_uri", "http://localhost/;csv", "URI on which to scrape HAProxy.")
		haProxyServerMetricFields = flag.String("haproxy.server_metric_fields", "", "If specified, only export the given csv fields. Comma-seperated list of numbers. See http://cbonte.github.io/haproxy-dconv/configuration-1.5.html#9.1")
		_                         = flag.Duration("haproxy.scrape_interval", 0, "DEPRECATED. Not used anymore.")
	)
	flag.Parse()

	exporter := NewExporter(*haProxyScrapeUri, *haProxyServerMetricFields)
	prometheus.MustRegister(exporter)

	log.Printf("Starting Server: %s", *listeningAddress)
	http.Handle(*metricsEndpoint, prometheus.Handler())
	log.Fatal(http.ListenAndServe(*listeningAddress, nil))
}
