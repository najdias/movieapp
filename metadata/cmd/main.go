package main

import (
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"time"

	_ "net/http/pprof"

	"github.com/uber-go/tally"
	"github.com/uber-go/tally/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"gopkg.in/yaml.v3"
	"movieexample.com/gen"
	"movieexample.com/metadata/internal/controller/metadata"
	grpchandler "movieexample.com/metadata/internal/handler/grpc"
	"movieexample.com/metadata/internal/repository/memory"
	"movieexample.com/pkg/discovery"
	"movieexample.com/pkg/discovery/consul"
	"movieexample.com/pkg/tracing"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	f, err := os.Open("base.yaml")
	if err != nil {
		logger.Fatal("Failed to open configuration", zap.Error(err))
	}
	var cfg config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		logger.Fatal("Failed to parse configuration", zap.Error(err))
	}
	port := cfg.API.Port

	simulateCPULoad := flag.Bool("simulate-cpu-load", false, "simulate CPU load for profiling")
	flag.Parse()
	if *simulateCPULoad {
		go heavyOperation()
	}

	go func() {
		if err := http.ListenAndServe(fmt.Sprintf(":%d", cfg.Profiler.Port), nil); err != nil {
			logger.Fatal("Failed to start profiler handler", zap.Error(err))
		}
	}()

	logger.Info("Starting the metadata service", zap.Int("port", port))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tp, err := tracing.NewJaegerProvider(cfg.Jaeger.URL, cfg.ServiceName)
	if err != nil {
		logger.Fatal("Failed to initialize Jaeger provider", zap.Error(err))
	}
	defer func() {
		if err := tp.Shutdown(ctx); err != nil {
			logger.Fatal("Failed to shut down Jaeger provider", zap.Error(err))
		}
	}()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	reporter := prometheus.NewReporter(prometheus.Options{})
	scope, closer := tally.NewRootScope(
		tally.ScopeOptions{
			Tags:           map[string]string{"service": cfg.ServiceName},
			CachedReporter: reporter,
		},
		10*time.Second,
	)
	defer closer.Close()
	http.Handle("/metrics", reporter.HTTPHandler())
	go func() {
		if err := http.ListenAndServe(fmt.Sprintf(":%d", cfg.Prometheus.MetricsPort), nil); err != nil {
			logger.Fatal("Failed to start the metrics handler", zap.Error(err))
		}
	}()

	counter := scope.Tagged(map[string]string{
		"service": cfg.ServiceName,
	}).Counter("service_started")
	counter.Inc(1)

	registry, err := consul.NewRegistry(cfg.Consul.URL)
	if err != nil {
		logger.Fatal("Failed to initialize registry with consul", zap.Error(err))
	}
	instanceID := discovery.GenerateInstanceID(cfg.ServiceName)
	if err := registry.Register(ctx, instanceID, cfg.ServiceName, fmt.Sprintf("%s:%d", cfg.ServiceName, port)); err != nil {
		logger.Fatal("Failed register gRPC instance in consul", zap.Error(err))
	}
	go func() {
		for {
			if err := registry.ReportHealthyState(instanceID, cfg.ServiceName); err != nil {
				logger.Error("Failed to report healthy state for gRPC", zap.Error(err))
			}
			time.Sleep(1 * time.Second)
		}
	}()
	serviceNameHTTP := cfg.ServiceName + "-http"
	instanceIDHTTP := discovery.GenerateInstanceID(serviceNameHTTP)
	if err := registry.Register(ctx, instanceIDHTTP, serviceNameHTTP, fmt.Sprintf("%s:%d", cfg.ServiceName, cfg.Prometheus.MetricsPort)); err != nil {
		logger.Fatal("Failed register HTTP instance in consul", zap.Error(err))
	}
	go func() {
		for {
			if err := registry.ReportHealthyState(instanceIDHTTP, serviceNameHTTP); err != nil {
				logger.Error("Failed to report healthy state for HTTP", zap.Error(err))
			}
			time.Sleep(1 * time.Second)
		}
	}()
	defer registry.Deregister(ctx, instanceID, cfg.ServiceName)
	repo := memory.New()
	ctrl := metadata.New(repo)
	h := grpchandler.New(ctrl)
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.ServiceName, port))
	if err != nil {
		logger.Fatal("Failed to listen", zap.Error(err))
	}
	srv := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	reflection.Register(srv)
	gen.RegisterMetadataServiceServer(srv, h)
	if err := srv.Serve(lis); err != nil {
		logger.Fatal("Failed to serve", zap.Error(err))
	}
}

func heavyOperation() {
	for {
		token := make([]byte, 1024)
		rand.Read(token)
		md5.New().Write(token)
	}
}
