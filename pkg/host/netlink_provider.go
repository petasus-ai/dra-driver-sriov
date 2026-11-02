package host

import (
	"github.com/vishvananda/netlink"
)

// BridgeVlan is one VLAN a bridge port is a member of.
type BridgeVlan struct {
	Vid int
	// Untagged is set for the port's PVID when it also leaves the port
	// untagged: the VLAN untagged frames on the wire belong to.
	Untagged bool
}

// NetlinkProvider wraps netlink library calls to allow mocking in unit tests.
type NetlinkProvider interface {
	// GetDevLinkDeviceEswitchMode returns the eswitch mode ("legacy" or
	// "switchdev") for the given PF PCI address via devlink.
	GetDevLinkDeviceEswitchMode(pciAddr string) (string, error)
	// GetLinkMaster returns the name and link type ("bond", "bridge", ...)
	// of the master the interface is enslaved to, or two empty strings when
	// it has none.
	GetLinkMaster(ifName string) (name, kind string, err error)
	// GetBridgePortVlans returns the VLANs a bridge port is a member of.
	GetBridgePortVlans(ifName string) ([]BridgeVlan, error)
}

type defaultNetlinkProvider struct{}

var _ NetlinkProvider = &defaultNetlinkProvider{}

func (defaultNetlinkProvider) GetDevLinkDeviceEswitchMode(pciAddr string) (string, error) {
	dev, err := netlink.DevLinkGetDeviceByName("pci", pciAddr)
	if err != nil {
		return "", err
	}
	return dev.Attrs.Eswitch.Mode, nil
}

func (defaultNetlinkProvider) GetLinkMaster(ifName string) (string, string, error) {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return "", "", err
	}
	masterIdx := link.Attrs().MasterIndex
	if masterIdx == 0 {
		return "", "", nil
	}
	master, err := netlink.LinkByIndex(masterIdx)
	if err != nil {
		return "", "", err
	}
	return master.Attrs().Name, master.Type(), nil
}

func (defaultNetlinkProvider) GetBridgePortVlans(ifName string) ([]BridgeVlan, error) {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return nil, err
	}
	vlanMap, err := netlink.BridgeVlanList()
	if err != nil {
		return nil, err
	}
	// A kernel interface index is an int32 by definition
	// (struct ifinfomsg.ifi_index), so this narrowing is exact.
	idx := int32(link.Attrs().Index) //nolint:gosec // ifindex is int32 in the kernel ABI
	vlans := make([]BridgeVlan, 0, len(vlanMap[idx]))
	for _, vi := range vlanMap[idx] {
		vlans = append(vlans, BridgeVlan{Vid: int(vi.Vid), Untagged: vi.PortVID() && vi.EngressUntag()})
	}
	return vlans, nil
}
