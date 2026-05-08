package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	rrcontext "github.com/roadrunner-server/context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestMiddleware_SpanEndsBeforeNextHandler proves the OTEL span covers only
// the prometheus middleware's own overhead, not the downstream handler time.
// A 50ms sleep in the next handler must NOT inflate the middleware span.
func TestMiddleware_SpanEndsBeforeNextHandler(t *testing.T) {
	const handlerDelay = 50 * time.Millisecond

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = tp.Shutdown(t.Context()) }()

	var p Plugin
	require.NoError(t, p.Init())

	// slow downstream handler
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(handlerDelay)
		w.WriteHeader(http.StatusOK)
	})

	handler := p.Middleware(next)

	// Build a request whose context carries a root span and the tracer-name key.
	ctx := context.WithValue(t.Context(), rrcontext.OtelTracerNameKey, "test-tracer")
	ctx, rootSpan := tp.Tracer("root").Start(ctx, "root-span")
	defer rootSpan.End()

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Force-flush so the syncer delivers all spans to the exporter.
	require.NoError(t, tp.ForceFlush(t.Context()))

	spans := exporter.GetSpans()

	// Find the prometheus middleware span.
	var promSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == pluginName {
			promSpan = &spans[i]
			break
		}
	}
	require.NotNil(t, promSpan, "expected a span named %q", pluginName)

	spanDuration := promSpan.EndTime.Sub(promSpan.StartTime)
	assert.Less(t, spanDuration, 10*time.Millisecond,
		"span should measure only middleware overhead (got %s); it must end before the next handler's %s sleep",
		spanDuration, handlerDelay,
	)
	assert.Equal(t, trace.SpanKindInternal, promSpan.SpanKind)
}

// TestMiddleware_NoSpanWithoutOtelContext verifies that no OTEL span is created
// when the request context does not carry OtelTracerNameKey.
func TestMiddleware_NoSpanWithoutOtelContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer func() { _ = tp.Shutdown(t.Context()) }()

	var p Plugin
	require.NoError(t, p.Init())

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := p.Middleware(next)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/no-otel", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.NoError(t, tp.ForceFlush(t.Context()))

	for _, s := range exporter.GetSpans() {
		assert.NotEqual(t, pluginName, s.Name, "no prometheus span should exist without OtelTracerNameKey in context")
	}
}
