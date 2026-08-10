package gateway

import (
	"testing"
)

func feedEstimateSamples(service *Service, model string, count, estimate, reported int) {
	for range count {
		service.observeTokenScale(model, 0, estimate, reported)
	}
}

func TestConfidentCalibrationUnlocksTheMeasuredScale(t *testing.T) {
	service := New(Options{ContextEnabled: true})
	// Prose-heavy model: the estimate runs 1.8x high, exactly the measured extreme.
	feedEstimateSamples(service, "model", 3, 18_000, 10_000)
	// Three samples are not evidence: the conservative clamp still caps at 1.5.
	if got := service.requestContextPolicy("model").EstimateScale; got != 1.5 {
		t.Fatalf("young scale = %v, want clamped 1.5", got)
	}
	feedEstimateSamples(service, "model", 9, 18_000, 10_000)
	// Twelve agreeing samples: the measurement takes over from the guess.
	if got := service.requestContextPolicy("model").EstimateScale; got < 1.75 || got > 1.85 {
		t.Fatalf("confident scale = %v, want ~1.8", got)
	}
}

func TestContradictorySamplesKeepTheConservativeClamp(t *testing.T) {
	service := New(Options{ContextEnabled: true})
	// Samples that keep disagreeing (alternating 1.8 and 0.9) hold spread high, so
	// sample count alone must never unlock the measured value.
	for index := range 20 {
		estimate := 18_000
		if index%2 == 1 {
			estimate = 9_000
		}
		service.observeTokenScale("model", 0, estimate, 10_000)
	}
	scale := service.tokenScaleFor("model")
	if scale.samples < calibConfidentSamples {
		t.Fatalf("samples = %d, want at least the confidence gate", scale.samples)
	}
	if scale.spread <= calibStableSpread {
		t.Fatalf("spread = %v, want above the stability bound", scale.spread)
	}
	if got := service.requestContextPolicy("model").EstimateScale; got > 1.5 {
		t.Fatalf("scale = %v, want clamped despite the sample count", got)
	}
}

func TestCalibrationPersistsAndSeedsAcrossRestart(t *testing.T) {
	var persisted []TokenCalibration
	first := New(Options{ContextEnabled: true, OnCalibration: func(cal TokenCalibration) {
		persisted = append(persisted, cal)
	}})
	feedEstimateSamples(first, "model", 16, 18_000, 10_000)
	if len(persisted) == 0 {
		t.Fatal("no calibration was persisted")
	}
	last := persisted[len(persisted)-1]
	// The heartbeat carries the confidence counters, not only the ratios.
	if last.Samples < calibConfidentSamples || last.EstimatePerToken < 1.7 {
		t.Fatalf("persisted = %+v", last)
	}

	// A fresh process seeded from what the previous one persisted is confident
	// immediately — nothing has to be re-earned.
	second := New(Options{ContextEnabled: true, LearnedCalibrations: []TokenCalibration{last}})
	if got := second.requestContextPolicy("model").EstimateScale; got < 1.7 {
		t.Fatalf("seeded scale = %v, want the measured value in force", got)
	}
}

func TestBytesOnlySampleTeachesRoutingWithoutFakingConfidence(t *testing.T) {
	service := New(Options{})
	// The passthrough path has real bytes and a real count but no estimate.
	service.observeTokenScale("claude-model", 80_000, 0, 10_000)
	scale := service.tokenScaleFor("claude-model")
	if scale.bytesPerToken != 8.0 {
		t.Fatalf("bytesPerToken = %v, want 8.0", scale.bytesPerToken)
	}
	if scale.samples != 0 {
		t.Fatalf("samples = %d; a bytes-only sample must not count toward estimate confidence", scale.samples)
	}
	if scale.estimatePerToken != fallbackEstimatePerToken {
		t.Fatalf("estimatePerToken = %v, want untouched fallback", scale.estimatePerToken)
	}
}
