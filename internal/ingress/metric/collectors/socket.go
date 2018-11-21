/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collectors

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"

	"github.com/golang/glog"
	jsoniter "github.com/json-iterator/go"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/util/sets"
)

type upstream struct {
	Latency        float64 `json:"uL"`
	ResponseLength float64 `json:"uResL"`
	ResponseTime   float64 `json:"uResT"`
	Status         string  `json:"uS"`
}

type socketData struct {
	Host   string `json:"h"`
	Status string `json:"s"`

	ResponseLength float64 `json:"resL"`

	RequestLength float64 `json:"rL"`
	RequestTime   float64 `json:"rT"`

	upstream

	Namespace string `json:"n"`
	Ingress   string `json:"i"`
	Service   string `json:"s"`
	Path      string `json:"p"`
}

// SocketCollector stores prometheus metrics and ingress meta-data
type SocketCollector struct {
	prometheus.Collector

	requestTime   *prometheus.HistogramVec
	requestLength *prometheus.HistogramVec

	responseTime   *prometheus.HistogramVec
	responseLength *prometheus.HistogramVec

	upstreamLatency *prometheus.SummaryVec

	bytesSent *prometheus.HistogramVec

	requests *prometheus.CounterVec

	listener net.Listener

	metricMapping map[string]interface{}

	hosts sets.String
}

var (
	requestTags = []string{
		"host",

		"status",

		"path",

		"namespace",
		"ingress",
		"service",
	}
)

// NewSocketCollector creates a new SocketCollector instance using
// the ingress watch namespace and class used by the controller
func NewSocketCollector(pod, namespace, class string) (*SocketCollector, error) {
	socket := "/tmp/prometheus-nginx.socket"
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}

	err = os.Chmod(socket, 0777)
	if err != nil {
		return nil, err
	}

	constLabels := prometheus.Labels{
		"controller_namespace": namespace,
		"controller_class":     class,
		"controller_pod":       pod,
	}

	sc := &SocketCollector{
		listener: listener,

		responseTime: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:        "response_duration_seconds",
				Help:        "The time spent on receiving the response from the upstream server",
				Namespace:   PrometheusNamespace,
				ConstLabels: constLabels,
			},
			requestTags,
		),
		responseLength: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:        "response_size",
				Help:        "The response length (including request line, header, and request body)",
				Namespace:   PrometheusNamespace,
				ConstLabels: constLabels,
			},
			requestTags,
		),

		requestTime: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:        "request_duration_seconds",
				Help:        "The request processing time in milliseconds",
				Namespace:   PrometheusNamespace,
				ConstLabels: constLabels,
			},
			requestTags,
		),
		requestLength: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:        "request_size",
				Help:        "The request length (including request line, header, and request body)",
				Namespace:   PrometheusNamespace,
				Buckets:     prometheus.LinearBuckets(10, 10, 10), // 10 buckets, each 10 bytes wide.
				ConstLabels: constLabels,
			},
			requestTags,
		),

		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "requests",
				Help:        "The total number of client requests.",
				Namespace:   PrometheusNamespace,
				ConstLabels: constLabels,
			},
			[]string{"ingress", "namespace", "status"},
		),

		bytesSent: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:        "bytes_sent",
				Help:        "The number of bytes sent to a client",
				Namespace:   PrometheusNamespace,
				Buckets:     prometheus.ExponentialBuckets(10, 10, 7), // 7 buckets, exponential factor of 10.
				ConstLabels: constLabels,
			},
			requestTags,
		),

		upstreamLatency: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:        "ingress_upstream_latency_seconds",
				Help:        "Upstream service latency per Ingress",
				Namespace:   PrometheusNamespace,
				ConstLabels: constLabels,
			},
			[]string{"ingress", "namespace", "service"},
		),
	}

	sc.metricMapping = map[string]interface{}{
		prometheus.BuildFQName(PrometheusNamespace, "", "request_duration_seconds"): sc.requestTime,
		prometheus.BuildFQName(PrometheusNamespace, "", "request_size"):             sc.requestLength,

		prometheus.BuildFQName(PrometheusNamespace, "", "response_duration_seconds"): sc.responseTime,
		prometheus.BuildFQName(PrometheusNamespace, "", "response_size"):             sc.responseLength,

		prometheus.BuildFQName(PrometheusNamespace, "", "bytes_sent"): sc.bytesSent,

		prometheus.BuildFQName(PrometheusNamespace, "", "ingress_upstream_latency_seconds"): sc.upstreamLatency,
	}

	return sc, nil
}

func (sc *SocketCollector) handleMessage(msg []byte) {
	glog.V(5).Infof("msg: %v", string(msg))

	// Unmarshal bytes
	var statsBatch []socketData
	err := jsoniter.ConfigCompatibleWithStandardLibrary.Unmarshal(msg, &statsBatch)
	if err != nil {
		glog.Errorf("Unexpected error deserializing JSON payload: %v. Payload:\n%v", err, string(msg))
		return
	}

	for _, stats := range statsBatch {
		if !sc.hosts.Has(stats.Host) {
			glog.V(3).Infof("skiping metric for host %v that is not being served", stats.Host)
			continue
		}

		requestLabels := prometheus.Labels{
			"host":      stats.Host,
			"status":    stats.Status,
			"path":      stats.Path,
			"namespace": stats.Namespace,
			"ingress":   stats.Ingress,
			"service":   stats.Service,
		}

		collectorLabels := prometheus.Labels{
			"namespace": stats.Namespace,
			"ingress":   stats.Ingress,
			"status":    stats.Status,
		}

		latencyLabels := prometheus.Labels{
			"namespace": stats.Namespace,
			"ingress":   stats.Ingress,
			"service":   stats.Service,
		}

		requestsMetric, err := sc.requests.GetMetricWith(collectorLabels)
		if err != nil {
			glog.Errorf("Error fetching requests metric: %v", err)
		} else {
			requestsMetric.Inc()
		}

		if stats.Latency != -1 {
			latencyMetric, err := sc.upstreamLatency.GetMetricWith(latencyLabels)
			if err != nil {
				glog.Errorf("Error fetching latency metric: %v", err)
			} else {
				latencyMetric.Observe(stats.Latency)
			}
		}

		if stats.RequestTime != -1 {
			requestTimeMetric, err := sc.requestTime.GetMetricWith(requestLabels)
			if err != nil {
				glog.Errorf("Error fetching request duration metric: %v", err)
			} else {
				requestTimeMetric.Observe(stats.RequestTime)
			}
		}

		if stats.RequestLength != -1 {
			requestLengthMetric, err := sc.requestLength.GetMetricWith(requestLabels)
			if err != nil {
				glog.Errorf("Error fetching request length metric: %v", err)
			} else {
				requestLengthMetric.Observe(stats.RequestLength)
			}
		}

		if stats.ResponseTime != -1 {
			responseTimeMetric, err := sc.responseTime.GetMetricWith(requestLabels)
			if err != nil {
				glog.Errorf("Error fetching upstream response time metric: %v", err)
			} else {
				responseTimeMetric.Observe(stats.ResponseTime)
			}
		}

		if stats.ResponseLength != -1 {
			bytesSentMetric, err := sc.bytesSent.GetMetricWith(requestLabels)
			if err != nil {
				glog.Errorf("Error fetching bytes sent metric: %v", err)
			} else {
				bytesSentMetric.Observe(stats.ResponseLength)
			}

			responseSizeMetric, err := sc.responseLength.GetMetricWith(requestLabels)
			if err != nil {
				glog.Errorf("Error fetching bytes sent metric: %v", err)
			} else {
				responseSizeMetric.Observe(stats.ResponseLength)
			}
		}
	}
}

// Start listen for connections in the unix socket and spawns a goroutine to process the content
func (sc *SocketCollector) Start() {
	for {
		conn, err := sc.listener.Accept()
		if err != nil {
			continue
		}

		go handleMessages(conn, sc.handleMessage)
	}
}

// Stop stops unix listener
func (sc *SocketCollector) Stop() {
	sc.listener.Close()
}

// RemoveMetrics deletes prometheus metrics from prometheus for ingresses and
// host that are not available anymore.
// Ref: https://godoc.org/github.com/prometheus/client_golang/prometheus#CounterVec.Delete
func (sc *SocketCollector) RemoveMetrics(ingresses []string, registry prometheus.Gatherer) {
	mfs, err := registry.Gather()
	if err != nil {
		glog.Errorf("Error gathering metrics: %v", err)
		return
	}

	// 1. remove metrics of removed ingresses
	glog.V(2).Infof("removing ingresses %v from metrics", ingresses)
	for _, mf := range mfs {
		metricName := mf.GetName()
		metric, ok := sc.metricMapping[metricName]
		if !ok {
			continue
		}

		toRemove := sets.NewString(ingresses...)
		for _, m := range mf.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, labelPair := range m.GetLabel() {
				labels[*labelPair.Name] = *labelPair.Value
			}

			// remove labels that are constant
			deleteConstants(labels)

			ns, ok := labels["namespace"]
			if !ok {
				continue
			}
			ing, ok := labels["ingress"]
			if !ok {
				continue
			}

			ingKey := fmt.Sprintf("%v/%v", ns, ing)
			if !toRemove.Has(ingKey) {
				continue
			}

			glog.V(2).Infof("Removing prometheus metric from histogram %v for ingress %v", metricName, ingKey)

			h, ok := metric.(*prometheus.HistogramVec)
			if ok {
				removed := h.Delete(labels)
				if !removed {
					glog.V(2).Infof("metric %v for ingress %v with labels not removed: %v", metricName, ingKey, labels)
				}
			}

			s, ok := metric.(*prometheus.SummaryVec)
			if ok {
				removed := s.Delete(labels)
				if !removed {
					glog.V(2).Infof("metric %v for ingress %v with labels not removed: %v", metricName, ingKey, labels)
				}
			}
		}
	}

}

// Describe implements prometheus.Collector
func (sc SocketCollector) Describe(ch chan<- *prometheus.Desc) {
	sc.requestTime.Describe(ch)
	sc.requestLength.Describe(ch)

	sc.requests.Describe(ch)

	sc.upstreamLatency.Describe(ch)

	sc.responseTime.Describe(ch)
	sc.responseLength.Describe(ch)

	sc.bytesSent.Describe(ch)
}

// Collect implements the prometheus.Collector interface.
func (sc SocketCollector) Collect(ch chan<- prometheus.Metric) {
	sc.requestTime.Collect(ch)
	sc.requestLength.Collect(ch)

	sc.requests.Collect(ch)

	sc.upstreamLatency.Collect(ch)

	sc.responseTime.Collect(ch)
	sc.responseLength.Collect(ch)

	sc.bytesSent.Collect(ch)
}

// SetHosts sets the hostnames that are being served by the ingress controller
// This set of hostnames is used to filter the metrics to be exposed
func (sc *SocketCollector) SetHosts(hosts sets.String) {
	sc.hosts = hosts
}

// handleMessages process the content received in a network connection
func handleMessages(conn io.ReadCloser, fn func([]byte)) {
	defer conn.Close()
	data, err := ioutil.ReadAll(conn)
	if err != nil {
		return
	}

	fn(data)
}

func deleteConstants(labels prometheus.Labels) {
	delete(labels, "controller_namespace")
	delete(labels, "controller_class")
	delete(labels, "controller_pod")
}
