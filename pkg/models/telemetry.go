package models

import (
	"errors"
	"strings"
	"time"
)

// TelemetryReading is the shared data contract between the edge collector
// and the cloud ingestor.
type TelemetryReading struct {
	DeviceID     string    `json:"device_id"`
	Timestamp    time.Time `json:"timestamp"`
	TemperatureC float64   `json:"temperature_c"`
	PressureHPA  float64   `json:"pressure_hpa"`
	HumidityPct  float64   `json:"humidity_pct"`
	Status       string    `json:"status"`
}

// Validate checks that all fields are present and within expected ranges.
// It does not touch time.Now(); callers are responsible for skew checks.
func (r TelemetryReading) Validate() error {
	if strings.TrimSpace(r.DeviceID) == "" {
		return errors.New("device_id is required")
	}
	if len(r.DeviceID) > 128 {
		return errors.New("device_id exceeds 128 characters")
	}
	if r.Timestamp.IsZero() {
		return errors.New("timestamp is required")
	}
	if r.TemperatureC < -50 || r.TemperatureC > 150 {
		return errors.New("temperature_c out of range [-50, 150]")
	}
	if r.PressureHPA < 800 || r.PressureHPA > 1200 {
		return errors.New("pressure_hpa out of range [800, 1200]")
	}
	if r.HumidityPct < 0 || r.HumidityPct > 100 {
		return errors.New("humidity_pct out of range [0, 100]")
	}
	if strings.TrimSpace(r.Status) == "" {
		return errors.New("status is required")
	}
	if len(r.Status) > 32 {
		return errors.New("status exceeds 32 characters")
	}
	return nil
}
