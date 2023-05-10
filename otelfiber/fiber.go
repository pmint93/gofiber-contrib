package otelfiber

import (
	"context"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	otelcontrib "go.opentelemetry.io/contrib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/global"
	"go.opentelemetry.io/otel/metric/instrument"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerKey           = "gofiber-contrib-tracer-fiber"
	instrumentationName = "github.com/gofiber/contrib/otelfiber"

	metricNameHttpServerDuration       = "http.server.duration"
	metricNameHttpServerRequestSize    = "http.server.request.size"
	metricNameHttpServerResponseSize   = "http.server.response.size"
	metricNameHttpServerActiveRequests = "http.server.active_requests"

	// Unit constants for deprecated metric units
	UnitDimensionless = "1"
	UnitBytes         = "By"
	UnitMilliseconds  = "ms"
)

// Middleware returns fiber handler which will trace incoming requests.
func Middleware(opts ...Option) fiber.Handler {
	cfg := config{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		instrumentationName,
		oteltrace.WithInstrumentationVersion(otelcontrib.SemVersion()),
	)

	if cfg.MeterProvider == nil {
		cfg.MeterProvider = global.MeterProvider()
	}
	meter := cfg.MeterProvider.Meter(
		instrumentationName,
		metric.WithInstrumentationVersion(otelcontrib.SemVersion()),
	)

	httpServerDuration, err := meter.Float64Histogram(metricNameHttpServerDuration, instrument.WithUnit(UnitMilliseconds), instrument.WithDescription("measures the duration inbound HTTP requests"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerRequestSize, err := meter.Int64Histogram(metricNameHttpServerRequestSize, instrument.WithUnit(UnitBytes), instrument.WithDescription("measures the size of HTTP request messages"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerResponseSize, err := meter.Int64Histogram(metricNameHttpServerResponseSize, instrument.WithUnit(UnitBytes), instrument.WithDescription("measures the size of HTTP response messages"))
	if err != nil {
		otel.Handle(err)
	}
	httpServerActiveRequests, err := meter.Int64UpDownCounter(metricNameHttpServerActiveRequests, instrument.WithUnit(UnitDimensionless), instrument.WithDescription("measures the number of concurrent HTTP requests that are currently in-flight"))
	if err != nil {
		otel.Handle(err)
	}

	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	if cfg.SpanNameFormatter == nil {
		cfg.SpanNameFormatter = defaultSpanNameFormatter
	}

	return func(c *fiber.Ctx) error {
		c.Locals(tracerKey, tracer)
		savedCtx, cancel := context.WithCancel(c.UserContext())

		start := time.Now()

		requestMetricsAttrs := httpServerMetricAttributesFromRequest(c, cfg)

		requestMetric := attributesToMetricOptions(requestMetricsAttrs)
		httpServerActiveRequests.Add(savedCtx, 1, requestMetric...)

		responseMetricAttrs := make([]attribute.KeyValue, len(requestMetricsAttrs))
		copy(responseMetricAttrs, requestMetricsAttrs)

		reqHeader := make(http.Header)
		c.Request().Header.VisitAll(func(k, v []byte) {
			reqHeader.Add(string(k), string(v))
		})

		ctx := cfg.Propagators.Extract(savedCtx, propagation.HeaderCarrier(reqHeader))

		opts := []oteltrace.SpanStartOption{
			oteltrace.WithAttributes(httpServerTraceAttributesFromRequest(c, cfg)...),
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		}

		// temporary set to c.Path() first
		// update with c.Route().Path after c.Next() is called
		// to get pathRaw
		spanName := utils.CopyString(c.Path())
		ctx, span := tracer.Start(ctx, spanName, opts...)
		defer span.End()

		// pass the span through userContext
		c.SetUserContext(ctx)

		// serve the request to the next middleware
		if err := c.Next(); err != nil {
			span.RecordError(err)
			// invokes the registered HTTP error handler
			// to get the correct response status code
			_ = c.App().Config().ErrorHandler(c, err)
		}

		// extract common attributes from response
		responseAttrs := append(
			semconv.HTTPAttributesFromHTTPStatusCode(c.Response().StatusCode()),
			semconv.HTTPRouteKey.String(c.Route().Path), // no need to copy c.Route().Path: route strings should be immutable across app lifecycle
		)

		requestSize := int64(len(c.Request().Body()))
		responseSize := int64(len(c.Response().Body()))

		defer func() {
			responseMetricAttrs = append(
				responseMetricAttrs,
				responseAttrs...)

			responRecord := attributesToRecordOptions(responseMetricAttrs)
			httpServerActiveRequests.Add(savedCtx, -1, requestMetric...)
			httpServerDuration.Record(savedCtx, float64(time.Since(start).Microseconds())/1000, responRecord...)
			httpServerRequestSize.Record(savedCtx, requestSize, responRecord...)
			httpServerResponseSize.Record(savedCtx, responseSize, responRecord...)

			c.SetUserContext(savedCtx)
			cancel()
		}()

		span.SetAttributes(
			append(
				responseAttrs,
				semconv.HTTPResponseContentLengthKey.Int64(responseSize),
			)...)
		span.SetName(cfg.SpanNameFormatter(c))

		spanStatus, spanMessage := semconv.SpanStatusFromHTTPStatusCodeAndSpanKind(c.Response().StatusCode(), oteltrace.SpanKindServer)
		span.SetStatus(spanStatus, spanMessage)

		return nil
	}
}

// defaultSpanNameFormatter is the default formatter for spans created with the fiber
// integration. Returns the route pathRaw
func defaultSpanNameFormatter(ctx *fiber.Ctx) string {
	return ctx.Route().Path
}

// convert addOption type
func attributesToMetricOptions(attrs []attribute.KeyValue) []metric.AddOption {
	options := make([]metric.AddOption, len(attrs))
	for i, attr := range attrs {
		options[i] = metric.WithAttributes(attr)
	}
	return options
}

// convert recordOption type
func attributesToRecordOptions(attrs []attribute.KeyValue) []metric.RecordOption {
	options := make([]metric.RecordOption, len(attrs))
	for i, attr := range attrs {
		options[i] = metric.WithAttributes(attr)
	}
	return options
}
