package models

import (
	"strings"
	"testing"
	"time"
)

func validReading() TelemetryReading {
	return TelemetryReading{
		DeviceID:     "sensor-001",
		Timestamp:    time.Now(),
		TemperatureC: 22.5,
		PressureHPA:  1013.0,
		HumidityPct:  55.0,
		Status:       "ok",
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*TelemetryReading)
		wantErr string
	}{
		{
			name:   "valid reading",
			mutate: func(_ *TelemetryReading) {},
		},
		{
			name:    "empty device_id",
			mutate:  func(r *TelemetryReading) { r.DeviceID = "" },
			wantErr: "device_id is required",
		},
		{
			name:    "whitespace-only device_id",
			mutate:  func(r *TelemetryReading) { r.DeviceID = "   " },
			wantErr: "device_id is required",
		},
		{
			name:    "device_id too long",
			mutate:  func(r *TelemetryReading) { r.DeviceID = strings.Repeat("x", 129) },
			wantErr: "device_id exceeds 128 characters",
		},
		{
			name:    "zero timestamp",
			mutate:  func(r *TelemetryReading) { r.Timestamp = time.Time{} },
			wantErr: "timestamp is required",
		},
		{
			name:    "temperature too low",
			mutate:  func(r *TelemetryReading) { r.TemperatureC = -51 },
			wantErr: "temperature_c out of range",
		},
		{
			name:    "temperature too high",
			mutate:  func(r *TelemetryReading) { r.TemperatureC = 151 },
			wantErr: "temperature_c out of range",
		},
		{
			name:    "temperature at lower boundary",
			mutate:  func(r *TelemetryReading) { r.TemperatureC = -50 },
			wantErr: "",
		},
		{
			name:    "temperature at upper boundary",
			mutate:  func(r *TelemetryReading) { r.TemperatureC = 150 },
			wantErr: "",
		},
		{
			name:    "pressure too low",
			mutate:  func(r *TelemetryReading) { r.PressureHPA = 799 },
			wantErr: "pressure_hpa out of range",
		},
		{
			name:    "pressure too high",
			mutate:  func(r *TelemetryReading) { r.PressureHPA = 1201 },
			wantErr: "pressure_hpa out of range",
		},
		{
			name:    "pressure at lower boundary",
			mutate:  func(r *TelemetryReading) { r.PressureHPA = 800 },
			wantErr: "",
		},
		{
			name:    "pressure at upper boundary",
			mutate:  func(r *TelemetryReading) { r.PressureHPA = 1200 },
			wantErr: "",
		},
		{
			name:    "humidity negative",
			mutate:  func(r *TelemetryReading) { r.HumidityPct = -1 },
			wantErr: "humidity_pct out of range",
		},
		{
			name:    "humidity above 100",
			mutate:  func(r *TelemetryReading) { r.HumidityPct = 100.1 },
			wantErr: "humidity_pct out of range",
		},
		{
			name:    "humidity at zero boundary",
			mutate:  func(r *TelemetryReading) { r.HumidityPct = 0 },
			wantErr: "",
		},
		{
			name:    "humidity at 100 boundary",
			mutate:  func(r *TelemetryReading) { r.HumidityPct = 100 },
			wantErr: "",
		},
		{
			name:    "empty status",
			mutate:  func(r *TelemetryReading) { r.Status = "" },
			wantErr: "status is required",
		},
		{
			name:    "whitespace-only status",
			mutate:  func(r *TelemetryReading) { r.Status = "   " },
			wantErr: "status is required",
		},
		{
			name:    "status too long",
			mutate:  func(r *TelemetryReading) { r.Status = strings.Repeat("x", 33) },
			wantErr: "status exceeds 32 characters",
		},
		{
			name:    "status exactly 32 characters",
			mutate:  func(r *TelemetryReading) { r.Status = strings.Repeat("x", 32) },
			wantErr: "",
		},
		{
			name:    "device_id exactly 128 characters",
			mutate:  func(r *TelemetryReading) { r.DeviceID = strings.Repeat("a", 128) },
			wantErr: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := validReading()
			tc.mutate(&r)
			err := r.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}
