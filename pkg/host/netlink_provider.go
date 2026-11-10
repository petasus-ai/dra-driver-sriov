package host

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

// from include/uapi/linux/if_link.h
const (
	iflaVfinfoList = 22
	iflaExtMask    = 29
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
	// GetVfPortGUIDs returns the InfiniBand port GUID the PF netdev reports
	// for each of its VFs, by VF index; 0 where none is set.
	GetVfPortGUIDs(pfName string) (map[int]uint64, error)
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

// GetVfPortGUIDs asks for the link with its VF information, as LinkByName
// does, and reads the IFLA_VF_IB_PORT_GUID of each VF, which netlink.VfInfo
// leaves out.
func (defaultNetlinkProvider) GetVfPortGUIDs(pfName string) (map[int]uint64, error) {
	iface, err := net.InterfaceByName(pfName)
	if err != nil {
		return nil, err
	}

	req := nl.NewNetlinkRequest(syscall.RTM_GETLINK, syscall.NLM_F_ACK)
	msg := nl.NewIfInfomsg(syscall.AF_UNSPEC)
	// A kernel interface index is an int32 by definition
	// (struct ifinfomsg.ifi_index), so this narrowing is exact.
	msg.Index = int32(iface.Index) //nolint:gosec // ifindex is int32 in the kernel ABI
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(iflaExtMask, nl.Uint32Attr(nl.RTEXT_FILTER_VF)))

	msgs, err := req.Execute(syscall.NETLINK_ROUTE, 0)
	if err != nil {
		return nil, err
	}
	if len(msgs) != 1 {
		return nil, fmt.Errorf("%d links answered for %s", len(msgs), pfName)
	}
	return parseVfPortGUIDs(msgs[0])
}

// parseVfPortGUIDs reads the VF port GUIDs out of an RTM_NEWLINK message:
// IFLA_VFINFO_LIST holds one IFLA_VF_INFO per VF, each with a struct
// ifla_vf_guid in IFLA_VF_IB_PORT_GUID when the netdev is an InfiniBand PF:
// the VF index (u32), padding to 8 bytes, the GUID as a number (u64), both
// in host byte order.
func parseVfPortGUIDs(msg []byte) (map[int]uint64, error) {
	if len(msg) < syscall.SizeofIfInfomsg {
		return nil, errors.New("truncated link message")
	}
	attrs, err := nl.ParseRouteAttr(msg[syscall.SizeofIfInfomsg:])
	if err != nil {
		return nil, err
	}

	guids := map[int]uint64{}
	for _, attr := range attrs {
		if attr.Attr.Type&nl.NLA_TYPE_MASK != iflaVfinfoList {
			continue
		}
		vfInfos, err := nl.ParseRouteAttr(attr.Value)
		if err != nil {
			return nil, err
		}
		for _, vfInfo := range vfInfos {
			if vfInfo.Attr.Type&nl.NLA_TYPE_MASK != nl.IFLA_VF_INFO {
				continue
			}
			vfAttrs, err := nl.ParseRouteAttr(vfInfo.Value)
			if err != nil {
				return nil, err
			}
			for _, vfAttr := range vfAttrs {
				if vfAttr.Attr.Type&nl.NLA_TYPE_MASK != nl.IFLA_VF_IB_PORT_GUID {
					continue
				}
				if len(vfAttr.Value) < nl.SizeofVfGUID {
					return nil, errors.New("truncated VF port GUID")
				}
				vf := nl.NativeEndian().Uint32(vfAttr.Value[0:4])
				guids[int(vf)] = nl.NativeEndian().Uint64(vfAttr.Value[8:16])
			}
		}
	}
	return guids, nil
}
