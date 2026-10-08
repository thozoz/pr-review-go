package config

import (
	"testing"
	"time"
)

// TestDeferValidationBounds: defer controls default into range and reject
// out-of-range values.
func TestDeferValidationBounds(t *testing.T) {
	c := &Config{}
	if err := c.ValidateDefer(); err != nil {
		t.Fatalf("ValidateDefer failed: %v", err)
	}
	if c.DeferMaxWait != DefaultDeferMaxWait {
		t.Errorf("DeferMaxWait = %v, want default %v", c.DeferMaxWait, DefaultDeferMaxWait)
	}
	if c.DeferPollInterval != DefaultDeferPollInterval {
		t.Errorf("DeferPollInterval = %v, want default %v", c.DeferPollInterval, DefaultDeferPollInterval)
	}

	c.DeferMaxWait = 30 * time.Second
	if err := c.ValidateDefer(); err == nil {
		t.Errorf("sub-minute DeferMaxWait must be rejected")
	}
	c.DeferMaxWait = DefaultDeferMaxWait
	c.DeferPollInterval = 10 * time.Minute
	if err := c.ValidateDefer(); err == nil {
		t.Errorf("over-long DeferPollInterval must be rejected")
	}
}
