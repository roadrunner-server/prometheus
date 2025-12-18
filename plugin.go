package prometheus

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	rrcontext "github.com/roadrunner-server/context"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	jprop "go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	pluginName string = "http_metrics"
	namespace  string = "rr_http"

	// should be in sync with the http/handler.go constants
	noWorkers string = "No-Workers"
	trueStr   string = "true"
)

type Plugin struct {
	writersPool sync.Pool
	prop        propagation.TextMapPropagator
	stopCh      chan struct{}

	queueSize       prometheus.Gauge
	noFreeWorkers   *prometheus.CounterVec
	requestCounter  *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	uptime          *prometheus.CounterVec
}

func (p *Plugin) Init() error {
	p.writersPool = sync.Pool{
		New: func() any {
			wr := new(writer)
			wr.code = -1
			return wr
		},
	}

	p.stopCh = make(chan struct{}, 1)
	p.queueSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "requests_queue",
		Help:      "Total number of queued requests.",
	})

	p.noFreeWorkers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "no_free_workers_total",
		Help:      "Total number of NoFreeWorkers occurrences.",
	}, nil)

	p.requestCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "request_total",
		Help:      "Total number of handled http requests after server restart.",
	}, []string{"status"})

	p.requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "request_duration_seconds",
			Help:      "HTTP request duration.",
			// Extended buckets to track slow requests (>10s)
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60},
		},
		[]string{"status"},
	)

	p.uptime = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "uptime_seconds",
		Help:      "Uptime in seconds",
	}, nil)

	p.prop = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}, jprop.Jaeger{})

	return nil
}

func (p *Plugin) Serve() chan error {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
				p.uptime.With(nil).Inc()
			}
		}
	}()
	return make(chan error, 1)
}

func (p *Plugin) Stop(context.Context) error {
	p.stopCh <- struct{}{}
	return nil
}

func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if val, ok := r.Context().Value(rrcontext.OtelTracerNameKey).(string); ok {
			tp := trace.SpanFromContext(r.Context()).TracerProvider()
			ctx, span := tp.Tracer(val, trace.WithSchemaURL(semconv.SchemaURL),
				trace.WithInstrumentationVersion(otelhttp.Version())).
				Start(r.Context(), pluginName, trace.WithSpanKind(trace.SpanKindServer))
			defer span.End()

			// inject
			p.prop.Inject(ctx, propagation.HeaderCarrier(r.Header))
			r = r.WithContext(ctx)
		}

		start := time.Now()

		// overwrite original rw, because we need to delete sensitive rr_newrelic headers
		rrWriter := p.getWriter(w)
		defer p.putWriter(rrWriter)

		p.queueSize.Inc()

		next.ServeHTTP(rrWriter, r)

		if w.Header().Get(noWorkers) == trueStr {
			p.noFreeWorkers.With(nil).Inc()
		}

		p.requestCounter.With(prometheus.Labels{
			"status": strconv.Itoa(rrWriter.code),
		}).Inc()

		p.requestDuration.With(prometheus.Labels{
			"status": strconv.Itoa(rrWriter.code),
		}).Observe(time.Since(start).Seconds())

		p.queueSize.Dec()
	})
}

func (p *Plugin) Name() string {
	return pluginName
}

func (p *Plugin) MetricsCollector() []prometheus.Collector {
	return []prometheus.Collector{p.requestCounter, p.requestDuration, p.queueSize, p.noFreeWorkers, p.uptime}
}

func (p *Plugin) getWriter(w http.ResponseWriter) *writer {
	wr := p.writersPool.Get().(*writer)
	wr.w = w
	return wr
}

func (p *Plugin) putWriter(w *writer) {
	w.code = -1
	w.w = nil
	p.writersPool.Put(w)
}
