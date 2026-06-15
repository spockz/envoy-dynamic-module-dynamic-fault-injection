package main

import (
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/spockz/envoy-latency-and-fault-distribution-simulation/internal/fault"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

type (
	// latencyFaultFilterConfigFactory implements [shared.HttpFilterConfigFactory].
	latencyFaultFilterConfigFactory struct {
		shared.EmptyHttpFilterConfigFactory
	}

	// latencyFaultFilterFactory implements [shared.HttpFilterFactory].
	latencyFaultFilterFactory struct {
		config    *fault.FilterConfig
		endpoints []endpointEntry
	}

	// endpointEntry holds a compiled endpoint with its response distribution.
	endpointEntry struct {
		match        fault.MatchConfig
		distribution *fault.ResponseDistribution
		loadBased    *fault.LoadBasedResponseDistribution
	}

	// latencyFaultFilter implements [shared.HttpFilter].
	// It operates as an upstream HTTP filter: it lets the request flow to the upstream,
	// then on response measures actual elapsed time and injects only the remaining delay
	// needed to match the target distribution.
	latencyFaultFilter struct {
		handle shared.HttpFilterHandle
		factory *latencyFaultFilterFactory

		// Populated during OnRequestHeaders.
		sample       fault.ResponseSample
		matched      bool
		requestStart time.Time

		shared.EmptyHttpFilter
	}
)

// Create implements [shared.HttpFilterConfigFactory].
func (f *latencyFaultFilterConfigFactory) Create(handle shared.HttpFilterConfigHandle, unparsedConfig []byte) (shared.HttpFilterFactory, error) {
	cfg, err := fault.ParseConfig(unparsedConfig)
	if err != nil {
		return nil, fmt.Errorf("latency_fault: failed to parse config: %w", err)
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	factory := &latencyFaultFilterFactory{
		config: cfg,
	}

	// Build per-endpoint distributions.
	for i, ep := range cfg.Endpoints {
		entry := endpointEntry{
			match: ep.Match,
		}

		// Build the simple response distribution if responses are configured.
		if len(ep.Responses) > 0 {
			dist, err := fault.NewResponseDistribution(ep.Responses, rng)
			if err != nil {
				return nil, fmt.Errorf("latency_fault: endpoint %d: failed to build response distribution: %w", i, err)
			}
			entry.distribution = dist
		}

		// Build the load-based distribution if configured.
		if ep.LoadBased != nil {
			lb, err := fault.NewLoadBasedResponseDistribution(
				ep.LoadBased.Healthy.Responses,
				ep.LoadBased.Healthy.ThresholdRPS,
				ep.LoadBased.TippingPoint.Responses,
				ep.LoadBased.TippingPoint.ThresholdRPS,
				ep.LoadBased.GreyZone,
				rng,
			)
			if err != nil {
				return nil, fmt.Errorf("latency_fault: endpoint %d: failed to build load-based distribution: %w", i, err)
			}
			entry.loadBased = lb
		}

		factory.endpoints = append(factory.endpoints, entry)
	}

	log.Printf("latency_fault: initialized with %d endpoints (upstream mode)", len(factory.endpoints))

	return factory, nil
}

// Create implements [shared.HttpFilterFactory].
func (f *latencyFaultFilterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &latencyFaultFilter{
		handle:  handle,
		factory: f,
	}
}

// headerMapAdapter adapts shared.HeaderMap to fault.HeaderGetter.
type headerMapAdapter struct {
	headers shared.HeaderMap
}

func (a *headerMapAdapter) GetOne(name string) string {
	return a.headers.GetOne(name)
}

// OnRequestHeaders is called when the request is flowing to the upstream.
// We sample from the distribution and record the start time, then let the request continue.
func (f *latencyFaultFilter) OnRequestHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	path := headers.GetOne(":path")

	// Find the matching endpoint and sample.
	adapter := &headerMapAdapter{headers: headers}
	for i := range f.factory.endpoints {
		ep := &f.factory.endpoints[i]
		if !fault.MatchRoute(ep.match, path, adapter) {
			continue
		}

		// Matched endpoint — sample a response.
		if ep.distribution != nil {
			f.sample = ep.distribution.Sample()
			f.matched = true
		} else if ep.loadBased != nil {
			// TODO: Feed actual RPS when tracking is implemented.
			f.sample = ep.loadBased.Sample(0)
			f.matched = true
		}
		break
	}

	// Record when the request was sent to upstream.
	if f.matched {
		f.requestStart = time.Now()
	}

	// Always let the request proceed to the upstream.
	return shared.HeadersStatusContinue
}

// OnResponseHeaders is called when the response arrives from the upstream.
// We calculate how much time the upstream actually took, then inject only
// the remaining delay (target - actual) to match the sampled distribution.
func (f *latencyFaultFilter) OnResponseHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	if !f.matched {
		return shared.HeadersStatusContinue
	}

	elapsed := time.Since(f.requestStart)
	remainingDelay := f.sample.Duration - elapsed
	if remainingDelay < 0 {
		remainingDelay = 0
	}

	// If the sampled status is an error (4xx/5xx), override the upstream response.
	if f.sample.Status >= 400 {
		if remainingDelay > 0 {
			// Delay, then send local error response.
			scheduler := f.handle.GetScheduler()
			sample := f.sample
			totalDuration := f.sample.Duration
			go func() {
				time.Sleep(remainingDelay)
				scheduler.Schedule(func() {
					f.handle.SendLocalResponse(
						uint32(sample.Status),
						[][2]string{
							{"Content-Type", "text/plain"},
							{"x-fault-injected", "abort"},
							{"x-fault-injected-delay", totalDuration.String()},
							{"x-fault-actual-upstream", elapsed.String()},
							{"x-fault-added-delay", remainingDelay.String()},
							{"x-fault-status", fmt.Sprintf("%d", sample.Status)},
						},
						[]byte(fmt.Sprintf("fault filter abort: %d\n", sample.Status)),
						"fault_abort",
					)
				})
			}()
			return shared.HeadersStatusStopAllAndBuffer
		}

		// No remaining delay needed — immediate abort.
		f.handle.SendLocalResponse(
			uint32(f.sample.Status),
			[][2]string{
				{"Content-Type", "text/plain"},
				{"x-fault-injected", "abort"},
				{"x-fault-injected-delay", f.sample.Duration.String()},
				{"x-fault-actual-upstream", elapsed.String()},
				{"x-fault-status", fmt.Sprintf("%d", f.sample.Status)},
			},
			[]byte(fmt.Sprintf("fault filter abort: %d\n", f.sample.Status)),
			"fault_abort",
		)
		return shared.HeadersStatusStop
	}

	// For success status codes: add metadata headers and delay if needed.
	headers.Set("x-fault-injected-delay", f.sample.Duration.String())
	headers.Set("x-fault-actual-upstream", elapsed.String())
	headers.Set("x-fault-status", fmt.Sprintf("%d", f.sample.Status))
	if remainingDelay > 0 {
		headers.Set("x-fault-added-delay", remainingDelay.String())
	}

	if remainingDelay > 0 {
		// Delay the response before continuing to downstream.
		scheduler := f.handle.GetScheduler()
		go func() {
			time.Sleep(remainingDelay)
			scheduler.Schedule(func() {
				f.handle.ContinueResponse()
			})
		}()
		return shared.HeadersStatusStopAllAndBuffer
	}

	// Upstream was already slow enough — no additional delay needed.
	return shared.HeadersStatusContinue
}
