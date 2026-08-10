package storage

import (
	"context"
	"testing"
)

func TestModelCalibrationRoundTrip(t *testing.T) {
	store := openTestStore(t)
	first := ModelCalibration{Model: "cx/gpt-5.6-luna", BytesPerToken: 4.4, EstimatePerToken: 1.8, Spread: 0.03, Samples: 14}
	if err := store.UpsertModelCalibration(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// A later write replaces the row rather than accumulating duplicates.
	second := first
	second.EstimatePerToken = 1.7
	second.Samples = 22
	if err := store.UpsertModelCalibration(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertModelCalibration(context.Background(), ModelCalibration{
		Model: "other", BytesPerToken: 5.0, EstimatePerToken: 0.9, Spread: 0.5, Samples: 2,
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := store.ListModelCalibrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("calibrations = %d, want 2", len(listed))
	}
	byModel := map[string]ModelCalibration{}
	for _, cal := range listed {
		byModel[cal.Model] = cal
	}
	luna := byModel["cx/gpt-5.6-luna"]
	if luna.EstimatePerToken != 1.7 || luna.Samples != 22 || luna.BytesPerToken != 4.4 || luna.Spread != 0.03 {
		t.Fatalf("luna = %+v", luna)
	}
	if luna.UpdatedAt.IsZero() {
		t.Fatal("updated_at not stamped")
	}
}

func TestModelCalibrationRejectsInvalid(t *testing.T) {
	store := openTestStore(t)
	cases := []ModelCalibration{
		{Model: "", BytesPerToken: 4, EstimatePerToken: 1},
		{Model: "m", BytesPerToken: 0, EstimatePerToken: 1},
		{Model: "m", BytesPerToken: 4, EstimatePerToken: -1},
		{Model: "m", BytesPerToken: 4, EstimatePerToken: 1, Samples: -1},
	}
	for index, cal := range cases {
		if err := store.UpsertModelCalibration(context.Background(), cal); err == nil {
			t.Fatalf("case %d accepted: %+v", index, cal)
		}
	}
}
