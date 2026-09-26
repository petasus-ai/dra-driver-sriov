package devicestate

import (
	"fmt"
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

		// The PF MTU caps what its VFs can carry. It is read once here and
		// stamped on the PF and every VF; a PF without a netdev has none to
		// report, and a read failure leaves the attribute out rather than
		// failing discovery.
		mtu := 0
		if pfNetName != "" {
			mtu, err = host.GetHelpers().GetNetDevMTU(pfNetName)
			if err != nil {
				logger.V(1).Info("Failed to read PF MTU, leaving pfMTU out",
					"address", device.Address, "interface", pfNetName, "err", err)
				mtu = 0
			}
		}

		logger.Info("Found SR-IOV PF device",
			"address", device.Address,
			"interface", pfNetName,
			"vendor", device.Vendor.ID,
			"device", device.Product.ID,
			"eswitchMode", eswitchMode,
			"numaNode", numaNode,
			"pcieRoot", pcieRoot,
			"linkType", linkType,
			"mtu", mtu)

		pfList = append(pfList, PFInfo{
			PciAddress:  device.Address,
			NetName:     pfNetName,
			VendorID:    device.Vendor.ID,
			DeviceID:    device.Product.ID,
			Address:     device.Address,
			EswitchMode: eswitchMode,
			PCIeRoot:    pcieRoot,
			LinkType:    linkType,
			NumaNode:    numaNode,
			MTU:         mtu,
		})
	}

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
			addPFNetdevAttributes(attributes, pfInfo)

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
// fields; vfID is omitted (no meaningful value), and PFName and pfMTU are
// omitted for netdev-less PFs.
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
	addPFNetdevAttributes(attributes, pfInfo)

	return resourceapi.Device{
		Name:       deviceName,
		Attributes: attributes,
	}
}

// addPFNetdevAttributes adds the attributes that come from the PF netdev,
// PFName and pfMTU, to a VF or PF entry. A netdev-less PF (vfio-bound) has
// neither to report, and an MTU that could not be read is left out.
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
