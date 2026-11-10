package devicestate

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jaypipes/pcidb"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/k8snetworkplumbingwg/dra-driver-sriov/pkg/consts"
	"github.com/k8snetworkplumbingwg/dra-driver-sriov/pkg/host"
	"github.com/k8snetworkplumbingwg/dra-driver-sriov/pkg/types"
)

type PFInfo struct {
	PciAddress  string
	NetName     string
	VendorID    string
	DeviceID    string
	Address     string
	EswitchMode string
	PCIeRoot    string
	LinkType    string
	NumaNode    string
	// MTU of the PF netdev; 0 when there is no netdev or it cannot be read.
	MTU int
	// LinkSpeedMbps of the PF netdev; 0 when there is no netdev, the link is
	// down, or it cannot be read.
	LinkSpeedMbps int
	// Uplink is what the uplink of a switchdev PF carries; nil in legacy
	// mode, without a netdev, or when it cannot be read.
	Uplink *host.UplinkVlans
	// PortGUID is the InfiniBand port GUID of the PF; empty for another
	// link type or when it is unknown.
	PortGUID string
	// MultiPort is set when the kernel shows more than one InfiniBand port
	// for the PF: it has no single port GUID and takes none from its VPD,
	// and its VFs take none set through it.
	MultiPort bool
}

func DiscoverSriovDevices() (types.AllocatableDevices, error) {
	logger := klog.LoggerWithName(klog.Background(), "DiscoverSriovDevices")
	pfList := []PFInfo{}
	resourceList := types.AllocatableDevices{}

	logger.Info("Starting SR-IOV device discovery")

	pci, err := host.GetHelpers().PCI()
	if err != nil {
		logger.Error(err, "Failed to get PCI info")
		return nil, fmt.Errorf("error getting PCI info: %v", err)
	}

	devices := pci.Devices
	if len(devices) == 0 {
		logger.Info("No PCI devices found")
		return nil, fmt.Errorf("could not retrieve PCI devices")
	}

	logger.Info("Found PCI devices", "count", len(devices))

	for _, device := range devices {
		logger.V(2).Info("Processing PCI device", "address", device.Address, "class", device.Class.ID)

		devClass, err := strconv.ParseInt(device.Class.ID, 16, 64)
		if err != nil {
			logger.Error(err, "Unable to parse device class, skipping device",
				"address", device.Address, "class", device.Class.ID)
			continue
		}
		if devClass != consts.NetClass {
			logger.V(3).Info("Skipping non-network device", "address", device.Address, "class", devClass)
			continue
		}

		// TODO: exclude devices used by host system
		if host.GetHelpers().IsSriovVF(device.Address) {
			logger.V(2).Info("Skipping VF device", "address", device.Address)
			continue
		}

		// A PF bound to a non-networking driver (e.g. vfio-pci for whole-PF
		// passthrough) has no netdev. Keep it as a candidate with an empty
		// name: it cannot host discoverable VFs but is itself advertisable
		// through a deviceType: pf filter.
		pfNetName := host.GetHelpers().TryGetPFInterfaceName(device.Address)
		if pfNetName == "" {
			logger.V(1).Info("Device has no netdev (non-networking driver?), keeping as PF candidate",
				"address", device.Address)
		}

		eswitchMode := host.GetHelpers().GetNicSriovMode(device.Address)

		// Get NUMA node information
		// -1 indicates NUMA is not supported/enabled (standard Linux convention)
		numaNode, err := host.GetHelpers().GetNumaNode(device.Address)
		if err != nil {
			logger.Error(err, "Failed to get NUMA node, using -1 (not supported)", "address", device.Address)
			numaNode = "-1"
		}

		// Get PCIe Root Complex information using upstream Kubernetes implementation
		pcieRoot, err := host.GetHelpers().GetPCIeRoot(device.Address)
		if err != nil {
			logger.Error(err, "Failed to get PCIe Root Complex", "address", device.Address)
			pcieRoot = "" // Leave empty if we can't determine it
		}

		// Get link type (ethernet, infiniband, etc.). GetLinkType resolves
		// through the netdev, which a vfio-bound PF does not have — fall
		// back to the PCI subclass in that case (0x00 ethernet, 0x07
		// infiniband).
		linkType, err := host.GetHelpers().GetLinkType(device.Address)
		if err != nil {
			linkType = linkTypeFromPCISubclass(device.Subclass)
			logger.V(1).Info("Falling back to PCI subclass for link type",
				"address", device.Address, "linkType", linkType, "getLinkTypeErr", err)
		}

		// The PF MTU caps what its VFs can carry, and the PF link speed is
		// the line rate they share. Both are read once here and stamped on
		// the PF and every VF; a PF without a netdev has neither to report,
		// and a read failure (a down link has no speed) leaves the attribute
		// out rather than failing discovery.
		mtu, linkSpeed := 0, 0
		if pfNetName != "" {
			mtu, err = host.GetHelpers().GetNetDevMTU(pfNetName)
			if err != nil {
				logger.V(1).Info("Failed to read PF MTU, leaving pfMTU out",
					"address", device.Address, "interface", pfNetName, "err", err)
				mtu = 0
			}
			linkSpeed, err = host.GetHelpers().GetNetDevSpeed(pfNetName)
			if err != nil {
				logger.V(1).Info("Failed to read PF link speed, leaving pfLinkSpeedMbps out",
					"address", device.Address, "interface", pfNetName, "err", err)
				linkSpeed = 0
			}
		}

		// A switchdev Ethernet VF reaches the fabric through its
		// representor's bridge, so it can only be put on a VLAN the bridge's
		// uplink carries. A legacy VF is tagged in the NIC and takes any
		// VLAN, and an InfiniBand VF is handed through with no VLAN at all,
		// so there is nothing to read for either. A PF that is no bridge port
		// (not yet provisioned) leaves the uplink attributes out.
		var uplink *host.UplinkVlans
		if pfNetName != "" && eswitchMode == consts.EswitchModeSwitchdev && linkType == consts.LinkTypeEthernet {
			uplink, err = host.GetHelpers().GetUplinkVlans(pfNetName)
			if err != nil {
				logger.V(1).Info("Failed to read PF uplink VLANs, leaving the uplink attributes out",
					"address", device.Address, "interface", pfNetName, "err", err)
				uplink = nil
			}
		}

		portGUID, multiPort := kernelPFPortGUID(logger, device.Address, linkType)

		logger.Info("Found SR-IOV PF device",
			"address", device.Address,
			"interface", pfNetName,
			"vendor", device.Vendor.ID,
			"device", device.Product.ID,
			"eswitchMode", eswitchMode,
			"numaNode", numaNode,
			"pcieRoot", pcieRoot,
			"linkType", linkType,
			"mtu", mtu,
			"linkSpeedMbps", linkSpeed,
			"uplink", uplink,
			"kernelPortGUID", portGUID)

		pfList = append(pfList, PFInfo{
			PciAddress:    device.Address,
			NetName:       pfNetName,
			VendorID:      device.Vendor.ID,
			DeviceID:      device.Product.ID,
			Address:       device.Address,
			EswitchMode:   eswitchMode,
			PCIeRoot:      pcieRoot,
			LinkType:      linkType,
			NumaNode:      numaNode,
			MTU:           mtu,
			LinkSpeedMbps: linkSpeed,
			Uplink:        uplink,
			PortGUID:      portGUID,
			MultiPort:     multiPort,
		})
	}

	resolveVPDPortGUIDs(logger, pfList)

	logger.Info("Processing SR-IOV PF devices", "pfCount", len(pfList))

	for _, pfInfo := range pfList {
		logger.V(1).Info("Getting VF list for PF", "pf", pfInfo.NetName, "address", pfInfo.Address)

		vfList, err := host.GetHelpers().GetVFList(pfInfo.Address)
		if err != nil {
			logger.Error(err, "Failed to get VF list for PF", "pf", pfInfo.NetName, "address", pfInfo.Address)
			return nil, fmt.Errorf("error getting VF list: %v", err)
		}

		logger.Info("Found VFs for PF", "pf", pfInfo.NetName, "vfCount", len(vfList))

		// Parse NUMA node value. Keep the actual value including -1 which indicates
		// NUMA is not supported/enabled (standard Linux convention).
		// This allows users to filter devices based on NUMA availability.
		numaNodeInt, err := strconv.ParseInt(pfInfo.NumaNode, 10, 64)
		if err != nil {
			logger.Error(err, "Failed to parse NUMA node, defaulting to -1",
				"pf", pfInfo.NetName, "numaNodeStr", pfInfo.NumaNode)
			numaNodeInt = -1
		}
		numaNodeIntPtr := ptr.To(numaNodeInt)

		// The PF is itself a potential device (whole-NIC passthrough).
		// Advertisement stays opt-in (deviceType: pf filter) and is gated on
		// the PF having no VFs, but the entry is always discovered so the
		// controller can evaluate it.
		pfDevice := buildPFDevice(pfInfo, len(vfList), numaNodeIntPtr)
		resourceList[pfDevice.Name] = pfDevice

		vfPortGUIDs := vfIBPortGUIDs(logger, pfInfo, vfList)

		for _, vfInfo := range vfList {
			deviceName := strings.ReplaceAll(vfInfo.PciAddress, ":", "-")
			deviceName = strings.ReplaceAll(deviceName, ".", "-")

			// Check RDMA capability for this VF
			rdmaCapable := host.GetHelpers().VerifyRDMACapability(vfInfo.PciAddress)

			logger.V(2).Info("Adding VF device to resource list",
				"deviceName", deviceName,
				"vfAddress", vfInfo.PciAddress,
				"vfID", vfInfo.VFID,
				"vfDeviceID", vfInfo.DeviceID,
				"pfDeviceID", pfInfo.DeviceID,
				"pf", pfInfo.NetName,
				"rdmaCapable", rdmaCapable)

			// Build device attributes
			attributes := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				consts.AttributeVendorID: {
					StringValue: ptr.To(pfInfo.VendorID),
				},
				consts.AttributeDeviceID: {
					StringValue: ptr.To(vfInfo.DeviceID),
				},
				consts.AttributePFDeviceID: {
					StringValue: ptr.To(pfInfo.DeviceID),
				},
				consts.AttributePciAddress: {
					StringValue: ptr.To(vfInfo.PciAddress),
				},
				consts.AttributeMultusDeviceID: {
					StringValue: ptr.To(vfInfo.PciAddress),
				},
				consts.AttributeDeviceType: {
					StringValue: ptr.To(consts.DeviceTypeVF),
				},
				consts.AttributeEswitchMode: {
					StringValue: ptr.To(pfInfo.EswitchMode),
				},
				consts.AttributeVFID: {
					IntValue: ptr.To(int64(vfInfo.VFID)),
				},
				// PCIe Root Complex (upstream Kubernetes standard) - for topology-aware scheduling
				consts.AttributePCIeRoot: {
					StringValue: ptr.To(pfInfo.PCIeRoot),
				},
				consts.AttributePfPciAddress: {
					StringValue: ptr.To(pfInfo.PciAddress),
				},
				// Standard Kubernetes PCI address attribute
				consts.AttributeStandardPciAddress: {
					StringValue: ptr.To(vfInfo.PciAddress),
				},
				// Link type (ethernet, infiniband, etc.)
				consts.AttributeLinkType: {
					StringValue: ptr.To(pfInfo.LinkType),
				},
				consts.AttributeRDMACapable: {
					BoolValue: ptr.To(rdmaCapable),
				},
				// compatibility attributes
				consts.AttributeNUMANode: {
					IntValue: numaNodeIntPtr,
				},
			}
			addStandardNUMANode(attributes, numaNodeIntPtr)
			addPFNetdevAttributes(attributes, pfInfo)
			addUplinkVlanAttributes(attributes, pfInfo.Uplink)
			addPortGUID(attributes, vfPortGUIDs[vfInfo.VFID])

			resourceList[deviceName] = resourceapi.Device{
				Name:       deviceName,
				Attributes: attributes,
			}
		}
	}

	logger.Info("SR-IOV device discovery completed", "totalDevices", len(resourceList))
	return resourceList, nil
}

// buildPFDevice builds the allocatable device entry for a PF candidate. The
// attribute layout mirrors the VF entries with self-referential parent
// fields; vfID is omitted (no meaningful value), PFName, pfMTU and
// pfLinkSpeedMbps are omitted for netdev-less PFs, and portGUID is present
// only on an InfiniBand PF whose port GUID is known.
func buildPFDevice(pfInfo PFInfo, numVFs int, numaNode *int64) resourceapi.Device {
	deviceName := strings.ReplaceAll(pfInfo.Address, ":", "-")
	deviceName = strings.ReplaceAll(deviceName, ".", "-")

	rdmaCapable := host.GetHelpers().VerifyRDMACapability(pfInfo.Address)

	attributes := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		consts.AttributeVendorID: {
			StringValue: ptr.To(pfInfo.VendorID),
		},
		consts.AttributeDeviceID: {
			StringValue: ptr.To(pfInfo.DeviceID),
		},
		consts.AttributePFDeviceID: {
			StringValue: ptr.To(pfInfo.DeviceID),
		},
		consts.AttributePciAddress: {
			StringValue: ptr.To(pfInfo.Address),
		},
		consts.AttributeMultusDeviceID: {
			StringValue: ptr.To(pfInfo.Address),
		},
		consts.AttributeDeviceType: {
			StringValue: ptr.To(consts.DeviceTypePF),
		},
		consts.AttributeNumVFs: {
			IntValue: ptr.To(int64(numVFs)),
		},
		consts.AttributeEswitchMode: {
			StringValue: ptr.To(pfInfo.EswitchMode),
		},
		consts.AttributePCIeRoot: {
			StringValue: ptr.To(pfInfo.PCIeRoot),
		},
		consts.AttributePfPciAddress: {
			StringValue: ptr.To(pfInfo.Address),
		},
		consts.AttributeStandardPciAddress: {
			StringValue: ptr.To(pfInfo.Address),
		},
		consts.AttributeLinkType: {
			StringValue: ptr.To(pfInfo.LinkType),
		},
		consts.AttributeRDMACapable: {
			BoolValue: ptr.To(rdmaCapable),
		},
		consts.AttributeNUMANode: {
			IntValue: numaNode,
		},
	}
	addStandardNUMANode(attributes, numaNode)
	addPFNetdevAttributes(attributes, pfInfo)
	addPortGUID(attributes, pfInfo.PortGUID)

	return resourceapi.Device{
		Name:       deviceName,
		Attributes: attributes,
	}
}

// kernelPFPortGUID returns the port GUID the kernel holds for an InfiniBand
// PF bound to its network/RDMA driver, "" when it holds none, and whether it
// shows more than one port for the PF.
func kernelPFPortGUID(logger klog.Logger, pciAddress, linkType string) (portGUID string, multiPort bool) {
	if linkType != consts.LinkTypeInfiniband {
		return "", false
	}

	guid, err := host.GetHelpers().GetIBPortGUID(pciAddress)
	if err == nil {
		return guid, false
	}
	if errors.Is(err, host.ErrMultiplePorts) {
		logger.V(1).Info("PF has more than one InfiniBand port, leaving portGUID out", "address", pciAddress, "err", err)
		return "", true
	}
	logger.V(2).Info("No kernel port GUID for PF", "address", pciAddress, "err", err)
	return "", false
}

// isMellanoxIBPF reports whether a PF is a Mellanox/NVIDIA InfiniBand one,
// the kind whose VPD may hold its port GUID.
func isMellanoxIBPF(pfInfo PFInfo) bool {
	return pfInfo.LinkType == consts.LinkTypeInfiniband && strings.EqualFold(pfInfo.VendorID, consts.VendorIDMellanox)
}

// needsVPDPortGUID reports whether a PF is left to its VPD for a port GUID:
// the kernel holds none, as for a PF bound to vfio-pci.
func needsVPDPortGUID(pfInfo PFInfo) bool {
	return isMellanoxIBPF(pfInfo) && pfInfo.PortGUID == "" && !pfInfo.MultiPort
}

// resolveVPDPortGUIDs gives the PFs that need one the port GUID derived from
// their VPD, all of them or none. A VPD names the adapter, not the PCI
// function it is read through, so a GUID is a PF's own only when no other PF
// of the node carries the same VPD. That is taken as proven when every PF
// the VPD rule applies to, the ones with a kernel GUID included, yields a
// GUID, and no two of them share a GUID, serial number or UUID, nor one of
// them the kernel GUID of another PF. A PF whose kernel GUID is not the one
// its VPD yields contradicts the rule on this node, and none is published.
// Nothing is read when no PF needs it.
func resolveVPDPortGUIDs(logger klog.Logger, pfList []PFInfo) {
	if !slices.ContainsFunc(pfList, needsVPDPortGUID) {
		return
	}

	derived := map[int]host.VPDPortGUID{}
	failed := []string{}
	for i, pfInfo := range pfList {
		if !isMellanoxIBPF(pfInfo) {
			continue
		}
		portGUID, err := host.GetHelpers().GetIBPortGUIDFromVPD(pfInfo.Address)
		switch {
		case errors.Is(err, host.ErrVPDNotApplicable):
			logger.V(2).Info("VPD port GUID rule does not apply to PF", "address", pfInfo.Address, "err", err)
		case err != nil:
			failed = append(failed, fmt.Sprintf("%s (%v)", pfInfo.Address, err))
		default:
			derived[i] = portGUID
		}
	}

	contradicted := []string{}
	for i, portGUID := range derived {
		if kernel := pfList[i].PortGUID; kernel != "" && kernel != portGUID.GUID {
			contradicted = append(contradicted, pfList[i].Address)
		}
	}
	slices.Sort(contradicted)

	collided := vpdPortGUIDCollisions(pfList, derived)
	if len(failed) > 0 || len(collided) > 0 || len(contradicted) > 0 {
		logger.Info("Leaving every VPD-derived portGUID out: a PF yielded none, PFs share a GUID, serial number or UUID, or a kernel GUID differs from the VPD's",
			"failed", failed, "collided", collided, "contradicted", contradicted)
		return
	}

	for i, portGUID := range derived {
		if needsVPDPortGUID(pfList[i]) {
			logger.V(1).Info("Port GUID of PF derived from its VPD", "address", pfList[i].Address, "portGUID", portGUID.GUID)
			pfList[i].PortGUID = portGUID.GUID
		}
	}
}

// vpdPortGUIDCollisions returns the addresses of the PFs whose VPD-derived
// GUID, serial number or UUID is that of another PF's VPD, or whose
// VPD-derived GUID is the kernel GUID of another PF, in ascending order.
func vpdPortGUIDCollisions(pfList []PFInfo, derived map[int]host.VPDPortGUID) []string {
	type identity struct{ kind, value string }
	owners := map[identity][]int{}
	own := func(kind, value string, i int) {
		owners[identity{kind, value}] = append(owners[identity{kind, value}], i)
	}
	for i, portGUID := range derived {
		own("guid", portGUID.GUID, i)
		own("uuid", portGUID.UUID, i)
		if portGUID.SerialNumber != "" {
			own("serial", portGUID.SerialNumber, i)
		}
	}

	collided := map[string]bool{}
	for _, indexes := range owners {
		if len(indexes) > 1 {
			for _, i := range indexes {
				collided[pfList[i].Address] = true
			}
		}
	}
	for i, portGUID := range derived {
		for j, other := range pfList {
			if j != i && other.PortGUID == portGUID.GUID {
				collided[pfList[i].Address] = true
				collided[other.Address] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(collided))
}

// vfIBPortGUIDs resolves the port GUIDs of the VFs of an InfiniBand PF, by VF
// ID; a VF whose GUID is unknown is left out. The kernel's own value of a VF
// bound to its network/RDMA driver comes first, then the GUID set
// administratively through the PF netdev, which is read at most once and
// only when a VF needs it. A VF the kernel shows more than one port for
// takes no GUID, and a PF it shows more than one port for is not asked.
func vfIBPortGUIDs(logger klog.Logger, pfInfo PFInfo, vfList []host.VFInfo) map[int]string {
	guids := map[int]string{}
	if pfInfo.LinkType != consts.LinkTypeInfiniband {
		return guids
	}

	var adminGUIDs map[int]string
	adminPending := pfInfo.NetName != "" && !pfInfo.MultiPort
	for _, vfInfo := range vfList {
		guid, err := host.GetHelpers().GetIBPortGUID(vfInfo.PciAddress)
		if err == nil {
			guids[vfInfo.VFID] = guid
			continue
		}
		if errors.Is(err, host.ErrMultiplePorts) {
			logger.V(1).Info("VF has more than one InfiniBand port, leaving portGUID out",
				"vfAddress", vfInfo.PciAddress, "err", err)
			continue
		}
		logger.V(2).Info("No kernel port GUID for VF", "vfAddress", vfInfo.PciAddress, "err", err)

		if adminPending {
			adminPending = false
			adminGUIDs, err = host.GetHelpers().GetVFIBPortGUIDs(pfInfo.Address, pfInfo.NetName)
			if err != nil {
				logger.V(2).Info("Failed to read the VF port GUIDs of the PF",
					"address", pfInfo.Address, "interface", pfInfo.NetName, "err", err)
			}
		}
		if adminGUID, ok := adminGUIDs[vfInfo.VFID]; ok {
			guids[vfInfo.VFID] = adminGUID
		} else {
			logger.V(2).Info("No port GUID for VF, leaving portGUID out", "vfAddress", vfInfo.PciAddress)
		}
	}
	return guids
}

// addPortGUID adds portGUID to a VF or PF entry whose InfiniBand port GUID is
// known.
func addPortGUID(attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, portGUID string) {
	if portGUID == "" {
		return
	}
	attributes[consts.AttributePortGUID] = resourceapi.DeviceAttribute{
		StringValue: ptr.To(portGUID),
	}
}

// addPFNetdevAttributes adds the attributes that come from the PF netdev,
// PFName, pfMTU and pfLinkSpeedMbps, to a VF or PF entry. A netdev-less PF
// (vfio-bound) has none to report, and an MTU or link speed that could not
// be read is left out.
func addPFNetdevAttributes(attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, pfInfo PFInfo) {
	if pfInfo.NetName != "" {
		attributes[consts.AttributePFName] = resourceapi.DeviceAttribute{
			StringValue: ptr.To(pfInfo.NetName),
		}
	}
	if pfInfo.MTU > 0 {
		attributes[consts.AttributePfMTU] = resourceapi.DeviceAttribute{
			IntValue: ptr.To(int64(pfInfo.MTU)),
		}
	}
	if pfInfo.LinkSpeedMbps > 0 {
		attributes[consts.AttributePfLinkSpeedMbps] = resourceapi.DeviceAttribute{
			IntValue: ptr.To(int64(pfInfo.LinkSpeedMbps)),
		}
	}
}

// addUplinkVlanAttributes adds what a VF's uplink carries: uplinkVlanFiltering,
// and on a VLAN-filtering bridge uplinkUntaggedVlan when the uplink carries a
// VLAN untagged and uplinkVlans when its VLAN list fits an attribute string.
// Nothing is added for an uplink that was not read (legacy mode, unreadable).
// PF entries get none: a whole PF is handed through with no VLAN of its own.
func addUplinkVlanAttributes(attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, uplink *host.UplinkVlans) {
	if uplink == nil {
		return
	}
	attributes[consts.AttributeUplinkVlanFiltering] = resourceapi.DeviceAttribute{
		BoolValue: ptr.To(uplink.VlanFiltering),
	}
	if !uplink.VlanFiltering {
		return
	}
	if uplink.UntaggedVlan > 0 {
		attributes[consts.AttributeUplinkUntaggedVlan] = resourceapi.DeviceAttribute{
			IntValue: ptr.To(int64(uplink.UntaggedVlan)),
		}
	}
	if vlans := formatVlanRanges(uplink.Vlans); vlans != "" && len(vlans) <= resourceapi.DeviceAttributeMaxValueLength {
		attributes[consts.AttributeUplinkVlans] = resourceapi.DeviceAttribute{
			StringValue: ptr.To(vlans),
		}
	}
}

// formatVlanRanges renders ascending VLAN IDs as comma-separated IDs and
// ranges of consecutive ones: [2200 2201 2202 3000] -> "2200-2202,3000".
func formatVlanRanges(vlans []int) string {
	parts := []string{}
	for i := 0; i < len(vlans); {
		j := i
		for j+1 < len(vlans) && vlans[j+1] == vlans[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", vlans[i], vlans[j]))
		} else {
			parts = append(parts, strconv.Itoa(vlans[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// addStandardNUMANode adds resource.kubernetes.io/numaNode to a VF or PF
// entry. A device with no NUMA affinity (-1) gets none: in a matchAttribute
// constraint the value would match every other NUMA-less device. The
// compatibility dra.net/numaNode keeps reporting -1.
func addStandardNUMANode(attributes map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, numaNode *int64) {
	if numaNode == nil || *numaNode < 0 {
		return
	}
	attributes[consts.AttributeStandardNUMANode] = resourceapi.DeviceAttribute{
		IntValue: ptr.To(*numaNode),
	}
}

// linkTypeFromPCISubclass maps a PCI network-controller subclass to a link
// type, for devices whose netdev-based link type cannot be read (e.g. a PF
// bound to vfio-pci).
func linkTypeFromPCISubclass(subclass *pcidb.Subclass) string {
	if subclass == nil {
		return consts.LinkTypeUnknown
	}
	switch subclass.ID {
	case "00":
		return consts.LinkTypeEthernet
	case "07":
		return consts.LinkTypeInfiniband
	default:
		return consts.LinkTypeUnknown
	}
}
