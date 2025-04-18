package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	gokitmetrics "github.com/go-kit/kit/metrics"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"github.com/traefik/traefik/v3/pkg/config/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const modeGRPC = "grpc"

// StatusSetter should be implemented by a service that, when the status of a
// registered target change, needs to be notified of that change.
type StatusSetter interface {
	SetStatus(ctx context.Context, childName string, up bool)
}

// StatusUpdater should be implemented by a service that, when its status
// changes (e.g. all if its children are down), needs to propagate upwards (to
// their parent(s)) that change.
type StatusUpdater interface {
	RegisterStatusUpdater(fn func(up bool)) error
}

type metricsHealthCheck interface {
	ServiceServerUpGauge() gokitmetrics.Gauge
}

type target struct {
	targetURL *url.URL
	name      string
}

type ServiceHealthChecker struct {
	balancer StatusSetter
	info     *runtime.ServiceInfo

	config            *dynamic.ServerHealthCheck
	interval          time.Duration
	unhealthyInterval time.Duration
	timeout           time.Duration

	metrics metricsHealthCheck

	client *http.Client

	healthyTargets   chan target
	unhealthyTargets chan target

	serviceName string
}

func NewServiceHealthChecker(ctx context.Context, metrics metricsHealthCheck, config *dynamic.ServerHealthCheck, service StatusSetter, info *runtime.ServiceInfo, transport http.RoundTripper, targets map[string]*url.URL, serviceName string) *ServiceHealthChecker {
	logger := log.Ctx(ctx)

	interval := time.Duration(config.Interval)
	if interval <= 0 {
		logger.Error().Msg("Health check interval smaller than zero, default value will be used instead.")
		interval = time.Duration(dynamic.DefaultHealthCheckInterval)
	}

	// If the unhealthyInterval option is not set, we use the interval option value,
	// to check the unhealthy targets as often as the healthy ones.
	var unhealthyInterval time.Duration
	if config.UnhealthyInterval == nil {
		unhealthyInterval = interval
	} else {
		unhealthyInterval = time.Duration(*config.UnhealthyInterval)
		if unhealthyInterval <= 0 {
			logger.Error().Msg("Health check unhealthy interval smaller than zero, default value will be used instead.")
			unhealthyInterval = time.Duration(dynamic.DefaultHealthCheckInterval)
		}
	}

	timeout := time.Duration(config.Timeout)
	if timeout <= 0 {
		logger.Error().Msg("Health check timeout smaller than zero, default value will be used instead.")
		timeout = time.Duration(dynamic.DefaultHealthCheckTimeout)
	}

	client := &http.Client{
		Transport: transport,
	}

	if config.FollowRedirects != nil && !*config.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	healthyTargets := make(chan target, len(targets))
	for name, targetURL := range targets {
		healthyTargets <- target{
			targetURL: targetURL,
			name:      name,
		}
	}
	unhealthyTargets := make(chan target, len(targets))

	return &ServiceHealthChecker{
		balancer:          service,
		info:              info,
		config:            config,
		interval:          interval,
		unhealthyInterval: unhealthyInterval,
		timeout:           timeout,
		healthyTargets:    healthyTargets,
		unhealthyTargets:  unhealthyTargets,
		serviceName:       serviceName,
		client:            client,
		metrics:           metrics,
	}
}

func (shc *ServiceHealthChecker) Launch(ctx context.Context) {
	go shc.healthcheck(ctx, shc.unhealthyTargets, shc.unhealthyInterval)

	shc.healthcheck(ctx, shc.healthyTargets, shc.interval)
}

func (shc *ServiceHealthChecker) healthcheck(ctx context.Context, targets chan target, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			// We collect the targets to check once for all,
			// to avoid rechecking a target that has been moved during the health check.
			var targetsToCheck []target
			hasMoreTargets := true
			for hasMoreTargets {
				select {
				case <-ctx.Done():
					return
				case target := <-targets:
					targetsToCheck = append(targetsToCheck, target)
				default:
					hasMoreTargets = false
				}
			}

			// Now we can check the targets.
			for _, target := range targetsToCheck {
				select {
				case <-ctx.Done():
					return
				default:
				}

				up := true
				serverUpMetricValue := float64(1)

				if err := shc.executeHealthCheck(ctx, shc.config, target.targetURL); err != nil {
					// The context is canceled when the dynamic configuration is refreshed.
					if errors.Is(err, context.Canceled) {
						return
					}

					log.Ctx(ctx).Warn().
						Str("targetURL", target.targetURL.String()).
						Err(err).
						Msg("Health check failed.")

					up = false
					serverUpMetricValue = float64(0)
				}

				shc.balancer.SetStatus(ctx, target.name, up)

				var statusStr string
				if up {
					statusStr = runtime.StatusUp
					shc.healthyTargets <- target
				} else {
					statusStr = runtime.StatusDown
					shc.unhealthyTargets <- target
				}

				shc.info.UpdateServerStatus(target.targetURL.String(), statusStr)

				shc.metrics.ServiceServerUpGauge().
					With("service", shc.serviceName, "url", target.targetURL.String()).
					Set(serverUpMetricValue)
			}
		}
	}
}

func (shc *ServiceHealthChecker) executeHealthCheck(ctx context.Context, config *dynamic.ServerHealthCheck, target *url.URL) error {
	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(shc.timeout))
	defer cancel()

	if config.Mode == modeGRPC {
		return shc.checkHealthGRPC(ctx, target)
	}
	return shc.checkHealthHTTP(ctx, target)
}

// checkHealthHTTP returns an error with a meaningful description if the health check failed.
// Dedicated to HTTP servers.
func (shc *ServiceHealthChecker) checkHealthHTTP(ctx context.Context, target *url.URL) error {
	req, err := shc.newRequest(ctx, target)
	if err != nil {
		return fmt.Errorf("create HTTP request: %w", err)
	}

	resp, err := shc.client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}

	defer resp.Body.Close()

	if shc.config.Status == 0 && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest) {
		return fmt.Errorf("received error status code: %v", resp.StatusCode)
	}

	if shc.config.Status != 0 && shc.config.Status != resp.StatusCode {
		return fmt.Errorf("received error status code: %v expected status code: %v", resp.StatusCode, shc.config.Status)
	}

	return nil
}

func (shc *ServiceHealthChecker) newRequest(ctx context.Context, target *url.URL) (*http.Request, error) {
	u, err := target.Parse(shc.config.Path)
	if err != nil {
		return nil, err
	}

	if len(shc.config.Scheme) > 0 {
		u.Scheme = shc.config.Scheme
	}

	if shc.config.Port != 0 {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(shc.config.Port))
	}

	req, err := http.NewRequestWithContext(ctx, shc.config.Method, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	if shc.config.Hostname != "" {
		req.Host = shc.config.Hostname
	}

	for k, v := range shc.config.Headers {
		req.Header.Set(k, v)
	}

	return req, nil
}

// checkHealthGRPC returns an error with a meaningful description if the health check failed.
// Dedicated to gRPC servers implementing gRPC Health Checking Protocol v1.
func (shc *ServiceHealthChecker) checkHealthGRPC(ctx context.Context, serverURL *url.URL) error {
	u, err := serverURL.Parse(shc.config.Path)
	if err != nil {
		return fmt.Errorf("failed to parse server URL: %w", err)
	}

	port := u.Port()
	if shc.config.Port != 0 {
		port = strconv.Itoa(shc.config.Port)
	}

	serverAddr := net.JoinHostPort(u.Hostname(), port)

	var opts []grpc.DialOption
	switch shc.config.Scheme {
	case "http", "h2c", "":
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.DialContext(ctx, serverAddr, opts...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("fail to connect to %s within %s: %w", serverAddr, shc.config.Timeout, err)
		}
		return fmt.Errorf("fail to connect to %s: %w", serverAddr, err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		if stat, ok := status.FromError(err); ok {
			switch stat.Code() {
			case codes.Unimplemented:
				return fmt.Errorf("gRPC server does not implement the health protocol: %w", err)
			case codes.DeadlineExceeded:
				return fmt.Errorf("gRPC health check timeout: %w", err)
			case codes.Canceled:
				return context.Canceled
			}
		}

		return fmt.Errorf("gRPC health check failed: %w", err)
	}

	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("received gRPC status code: %v", resp.GetStatus())
	}

	return nil
}
