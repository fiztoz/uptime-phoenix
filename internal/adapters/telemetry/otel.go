// Package telemetry provides optional OpenTelemetry bootstrap for Phoenix.
package telemetry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config holds OTLP exporter settings.
type Config struct {
	// Endpoint is OTEL_EXPORTER_OTLP_ENDPOINT (host:port or full URL path).
	Endpoint string
	// ServiceName defaults to phoenix when empty.
	ServiceName string
}

// Init configures the global TracerProvider when endpoint is non-empty.
// Returns a shutdown function that should be called on process exit.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	ep := strings.TrimSpace(cfg.Endpoint)
	if ep == "" {
		return func(context.Context) error { return nil }, nil
	}

	svc := cfg.ServiceName
	if svc == "" {
		svc = "phoenix"
	}

	opts := []otlptracehttp.Option{}
	if strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://") {
		opts = append(opts, otlptracehttp.WithEndpointURL(ep))
	} else {
		opts = append(opts, otlptracehttp.WithEndpoint(ep))
		if os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true" {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(svc),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}
	return shutdown, nil
}
