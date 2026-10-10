package core

import "time"

type PlatformHealthInfo struct {
	Name           string    `json:"name"`
	Connected      bool      `json:"connected"`
	Degraded       bool      `json:"degraded"`
	DegradedReason string    `json:"degraded_reason,omitempty"`
	DegradedSince  time.Time `json:"degraded_since,omitempty"`
}
type PlatformHealth interface{ PlatformHealth() PlatformHealthInfo }
