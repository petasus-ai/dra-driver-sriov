package v1alpha1

import (
	"fmt"
	"net"
)

// Validate ensures that GpuConfig has a valid set of values.
func (c *VfConfig) Validate() error {
	if c.Driver == "" {
		return fmt.Errorf("no driver set")
	}
	if c.NetAttachDefName == "" {
		return fmt.Errorf("no net attach def name set")
	}
	if c.Mac != "" {
		if _, err := net.ParseMAC(c.Mac); err != nil {
			return fmt.Errorf("invalid mac %q: %v", c.Mac, err)
		}
	}

	return nil
}
