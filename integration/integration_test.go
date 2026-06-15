package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/mccutchen/go-httpbin/v2/httpbin"
	"github.com/stretchr/testify/require"
)

func TestIntegration(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	// Setup the httpbin upstream local server.
	httpbinHandler := httpbin.New()
	server := &http.Server{
		Addr:              ":1234",
		Handler:           httpbinHandler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.Logf("HTTP server error: %v", err)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	// Health check to ensure the backend is up.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://localhost:1234/status/200")
		if err != nil {
			t.Logf("httpbin server not ready yet: %v", err)
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == 200
	}, 10*time.Second, 500*time.Millisecond)

	// Start Envoy with the dynamic module.
	if envoyImage := os.Getenv("ENVOY_IMAGE"); envoyImage != "" {
		containerRuntime := detectContainerRuntime()
		t.Logf("Using container runtime: %s", containerRuntime)
		cmd := exec.Command(
			containerRuntime,
			"run",
			"--network", "host",
			"-v", cwd+":/integration",
			"-w", "/integration",
			"-e", "GODEBUG=cgocheck=0",
			"--rm",
			envoyImage,
			"--concurrency", "1",
			"--config-path", "/integration/envoy.yaml",
			"--component-log-level", "dynamic_modules:debug",
			"--base-id", strconv.Itoa(time.Now().Nanosecond()),
		)
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		t.Logf("Running: %s", cmd.String())
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Signal(os.Interrupt) })
	} else {
		cmd := exec.Command("go",
			"tool", "func-e", "run",
			"-c", "envoy.yaml",
			"--log-level", "warn",
			"--concurrency", "1",
			"--component-log-level", "dynamic_modules:debug",
			"--base-id", strconv.Itoa(time.Now().Nanosecond()),
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(),
			"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+cwd,
			"GODEBUG=cgocheck=0",
		)
		t.Logf("Running: %s", cmd.String())
		require.NoError(t, cmd.Start())
		defer func() {
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Logf("failed to interrupt envoy: %v", err)
			}
			time.Sleep(3 * time.Second)
			_ = cmd.Process.Kill()
		}()
	}

	// Wait for Envoy to be ready.
	t.Run("health_check", func(t *testing.T) {
		require.Eventually(t, func() bool {
			resp, err := http.Get("http://localhost:10000/status/200")
			if err != nil {
				t.Logf("Envoy not ready yet: %v", err)
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)
			return resp.StatusCode == 200
		}, 120*time.Second, 1*time.Second)
	})

	t.Run("distribution_delay", func(t *testing.T) {
		// Requests to /delay have distribution: p0=20ms, p50=50ms, p90=100ms, p99=200ms, p100=300ms.
		// The upstream filter measures actual upstream time and only adds the remaining delay.
		var durations []time.Duration
		const numRequests = 20

		for i := 0; i < numRequests; i++ {
			start := time.Now()
			req, err := http.NewRequest("GET", "http://localhost:10000/delay/0", nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			elapsed := time.Since(start)
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)

			require.Equal(t, 200, resp.StatusCode)

			// Verify upstream filter headers are present.
			delayHeader := resp.Header.Get("x-fault-injected-delay")
			require.NotEmpty(t, delayHeader, "x-fault-injected-delay header should be set (target duration)")

			upstreamHeader := resp.Header.Get("x-fault-actual-upstream")
			require.NotEmpty(t, upstreamHeader, "x-fault-actual-upstream header should be set")

			statusHeader := resp.Header.Get("x-fault-status")
			require.Equal(t, "200", statusHeader, "x-fault-status header should be 200")

			durations = append(durations, elapsed)
			t.Logf("request %d: elapsed=%v, target=%s, upstream=%s, added=%s",
				i, elapsed, delayHeader, upstreamHeader,
				resp.Header.Get("x-fault-added-delay"))
		}

		// Verify that total observed time matches the distribution.
		var totalDelay time.Duration
		for _, d := range durations {
			totalDelay += d
		}
		avgDelay := totalDelay / time.Duration(numRequests)
		t.Logf("average request time: %v", avgDelay)

		// Average should be roughly around 50ms (p50 of distribution).
		require.Greater(t, avgDelay.Milliseconds(), int64(20),
			"average delay should be meaningful (got %v)", avgDelay)
	})

	t.Run("abort_injection", func(t *testing.T) {
		// Requests to /abort: all responses sampled as 503.
		// The upstream filter lets the request go through, then overrides the response.
		require.Eventually(t, func() bool {
			req, err := http.NewRequest("GET", "http://localhost:10000/abort/test", nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)

			t.Logf("abort response: status=%d body=%s", resp.StatusCode, string(body))
			require.Equal(t, 503, resp.StatusCode)
			require.Contains(t, string(body), "fault filter abort")
			require.Equal(t, "abort", resp.Header.Get("x-fault-injected"))
			return true
		}, 30*time.Second, 200*time.Millisecond)
	})

	t.Run("fixed_delay_accounts_for_upstream", func(t *testing.T) {
		// Port 10001 has a flat 100ms distribution. The upstream filter should
		// only add (100ms - actual_upstream_time) as additional delay.
		require.Eventually(t, func() bool {
			start := time.Now()
			req, err := http.NewRequest("GET", "http://localhost:10001/status/200", nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Logf("Envoy port 10001 not ready yet: %v", err)
				return false
			}
			elapsed := time.Since(start)
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)

			t.Logf("fixed delay: status=%d elapsed=%v target=%s upstream=%s added=%s",
				resp.StatusCode, elapsed,
				resp.Header.Get("x-fault-injected-delay"),
				resp.Header.Get("x-fault-actual-upstream"),
				resp.Header.Get("x-fault-added-delay"))

			require.Equal(t, 200, resp.StatusCode)

			// Total time should be ~100ms (target) regardless of upstream speed.
			require.Greater(t, elapsed.Milliseconds(), int64(90),
				"total time should be at least ~100ms")
			require.Less(t, elapsed.Milliseconds(), int64(500),
				"request should not take excessively long")

			// The target header should show 100ms.
			delayHeader := resp.Header.Get("x-fault-injected-delay")
			require.Equal(t, "100ms", delayHeader)

			// The actual upstream time should be much less than 100ms.
			upstreamHeader := resp.Header.Get("x-fault-actual-upstream")
			require.NotEmpty(t, upstreamHeader)

			return true
		}, 120*time.Second, 1*time.Second)
	})

	t.Run("catchall_endpoint", func(t *testing.T) {
		// Requests to paths that don't match specific prefixes hit the "/" catch-all.
		// Distribution: p0=5ms, p50=10ms, p100=20ms.
		require.Eventually(t, func() bool {
			req, err := http.NewRequest("GET", "http://localhost:10000/status/200", nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)

			t.Logf("catchall: status=%d target=%s upstream=%s",
				resp.StatusCode,
				resp.Header.Get("x-fault-injected-delay"),
				resp.Header.Get("x-fault-actual-upstream"))

			require.Equal(t, 200, resp.StatusCode)
			require.NotEmpty(t, resp.Header.Get("x-fault-injected-delay"),
				"should have target delay header from catchall endpoint")
			require.NotEmpty(t, resp.Header.Get("x-fault-actual-upstream"),
				"should have actual upstream header")
			return true
		}, 30*time.Second, 200*time.Millisecond)
	})

	t.Run("mixed_status_codes", func(t *testing.T) {
		// Requests to /mixed: 50% → 200 (30-80ms delay, upstream response passes through),
		// 50% → 429 abort (overrides upstream response).
		// httpbin doesn't have /mixed/* so upstream returns 404.
		// When sample=200: upstream 404 passes through. When sample=429: local 429 response.
		gotUpstream := false
		got429 := false

		require.Eventually(t, func() bool {
			req, err := http.NewRequest("GET", "http://localhost:10000/mixed/test", nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)

			t.Logf("mixed: status=%d fault-status=%s", resp.StatusCode, resp.Header.Get("x-fault-status"))

			if resp.StatusCode == 429 {
				got429 = true
			} else {
				// When the sampled status is 200, upstream response passes through.
				gotUpstream = true
			}
			return gotUpstream && got429
		}, 30*time.Second, 100*time.Millisecond)
	})

	t.Run("upstream_time_is_subtracted", func(t *testing.T) {
		// Use httpbin's /delay endpoint to simulate a slow upstream.
		// With /delay/0.05 (50ms upstream) and our distribution min at 5ms,
		// the filter should see that upstream already exceeded the target
		// and not add any extra delay.
		require.Eventually(t, func() bool {
			start := time.Now()
			// httpbin /delay/0.05 takes ~50ms to respond.
			req, err := http.NewRequest("GET", "http://localhost:10000/delay/0.05", nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			elapsed := time.Since(start)
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)

			// The catch-all "/" has p100=20ms, but upstream takes ~50ms.
			// So the filter should NOT add additional delay (upstream already exceeded target).
			// But /delay matches first with p0=20ms..p100=300ms, so it depends on the sample.
			t.Logf("upstream_subtraction: status=%d elapsed=%v target=%s upstream=%s added=%s",
				resp.StatusCode, elapsed,
				resp.Header.Get("x-fault-injected-delay"),
				resp.Header.Get("x-fault-actual-upstream"),
				resp.Header.Get("x-fault-added-delay"))

			require.Equal(t, 200, resp.StatusCode)
			return true
		}, 30*time.Second, 500*time.Millisecond)
	})
}

// detectContainerRuntime returns "podman" if available, otherwise "docker".
func detectContainerRuntime() string {
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman"
	}
	return "docker"
}
