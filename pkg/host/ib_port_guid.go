package host

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/k8snetworkplumbingwg/dra-driver-sriov/pkg/consts"
)

const (
	// rdmaLinkLayerInfiniband is ports/<n>/link_layer of an InfiniBand port.
	rdmaLinkLayerInfiniband = "InfiniBand"
	// rdmaSinglePort is the only port of a single-port RDMA device.
	rdmaSinglePort = "1"

	gidBytes          = 16
	ipoibAddressBytes = 20

	// mellanoxConnectX4DeviceID is the first Mellanox PCI device ID of the
	// adapters that give every port its own PCI function: ConnectX-4 and
	// later. The IDs below it (mlx4 ConnectX-3 and older, Connect-IB) and
	// the range of the older InfiniHost and ConnectX IDs belong to adapters
	// that can carry two ports on one function.
	mellanoxConnectX4DeviceID    = 0x1013
	mellanoxLegacyDeviceIDsFirst = 0x5a44
	mellanoxLegacyDeviceIDsLast  = 0x676e
)

var (
	pciAddressRegexp = regexp.MustCompile(`^[0-9a-fA-F]{4,8}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

	// ErrMultiplePorts marks a PCI function the kernel shows more than one
	// InfiniBand port for. It has no single port GUID, and no other source
	// may supply one.
	ErrMultiplePorts = errors.New("function does not have exactly one InfiniBand port")

	// ErrVPDNotApplicable marks a PCI function the VPD rule is not applied
	// to: its VPD is not read and says nothing about any port GUID.
	ErrVPDNotApplicable = errors.New("VPD port GUID rule does not apply")

	errNoRDMADevice = errors.New("no RDMA device")
)

// GetIBPortGUID returns the InfiniBand port GUID the kernel holds for a PCI
// function bound to its network/RDMA driver: GID 0 of the only port of its
// RDMA device, else the address of its IPoIB netdev. A function with more
// than one port fails with ErrMultiplePorts.
func (h *Host) GetIBPortGUID(pciAddr string) (string, error) {
	if !pciAddressRegexp.MatchString(pciAddr) {
		return "", fmt.Errorf("invalid PCI address %q", pciAddr)
	}

	guid, rdmaErr := h.rdmaPortGUID(pciAddr)
	if rdmaErr == nil {
		return guid, nil
	}
	if errors.Is(rdmaErr, ErrMultiplePorts) {
		return "", fmt.Errorf("no single port GUID for %s: %w", pciAddr, rdmaErr)
	}

	guid, netErr := h.ipoibPortGUID(pciAddr)
	if netErr == nil {
		return guid, nil
	}
	if errors.Is(netErr, ErrMultiplePorts) {
		return "", fmt.Errorf("no single port GUID for %s: %w", pciAddr, netErr)
	}
	return "", fmt.Errorf("no kernel port GUID for %s: %w", pciAddr, errors.Join(rdmaErr, netErr))
}

// singleIBPort returns the sysfs directory of the one InfiniBand port of a
// PCI function, /sys/class/infiniband/<device>/ports/1. A function with more
// than one RDMA device, or an RDMA device with more than one port, fails
// with ErrMultiplePorts.
func (h *Host) singleIBPort(pciAddr string) (rdmaDevice, portDir string, err error) {
	rdmaDevices := h.rdmaProvider.GetRdmaDevicesForPcidev(pciAddr)
	if len(rdmaDevices) == 0 {
		return "", "", errNoRDMADevice
	}
	if len(rdmaDevices) > 1 {
		return "", "", fmt.Errorf("%d RDMA devices: %w", len(rdmaDevices), ErrMultiplePorts)
	}
	rdmaDevice = filepath.Base(rdmaDevices[0])

	portsDir := buildSysPath(filepath.Join(consts.SysClassInfiniband, rdmaDevice, "ports"))
	ports, err := os.ReadDir(portsDir)
	if err != nil {
		return "", "", fmt.Errorf("failed to read the ports of RDMA device %s: %w", rdmaDevice, err)
	}
	if len(ports) != 1 || ports[0].Name() != rdmaSinglePort {
		return "", "", fmt.Errorf("RDMA device %s has %d ports: %w", rdmaDevice, len(ports), ErrMultiplePorts)
	}
	portDir = filepath.Join(portsDir, rdmaSinglePort)

	linkLayer, err := os.ReadFile(filepath.Join(portDir, "link_layer")) /* #nosec G304 */
	if err != nil {
		return "", "", fmt.Errorf("failed to read the link layer of RDMA device %s: %w", rdmaDevice, err)
	}
	if strings.TrimSpace(string(linkLayer)) != rdmaLinkLayerInfiniband {
		return "", "", fmt.Errorf("RDMA device %s has link layer %s", rdmaDevice, strings.TrimSpace(string(linkLayer)))
	}
	return rdmaDevice, portDir, nil
}

// rdmaPortGUID reads the port GUID from GID 0 of the function's InfiniBand
// port, ports/1/gids/0, whose last 8 bytes are the port GUID.
func (h *Host) rdmaPortGUID(pciAddr string) (string, error) {
	rdmaDevice, portDir, err := h.singleIBPort(pciAddr)
	if err != nil {
		return "", err
	}

	gid, err := os.ReadFile(filepath.Join(portDir, "gids", "0")) /* #nosec G304 */
	if err != nil {
		return "", fmt.Errorf("failed to read GID 0 of RDMA device %s: %w", rdmaDevice, err)
	}
	guid, err := guidFromHardwareAddress(string(gid), gidBytes)
	if err != nil {
		return "", fmt.Errorf("GID 0 of RDMA device %s: %w", rdmaDevice, err)
	}
	return guid, nil
}

// ipoibPortGUID reads the port GUID from the function's IPoIB netdevs,
// /sys/bus/pci/devices/<address>/net/<name>/address, whose last 8 bytes are
// the port GUID. Child (P_Key) interfaces share their parent's; netdevs that
// disagree belong to different ports and fail with ErrMultiplePorts.
func (h *Host) ipoibPortGUID(pciAddr string) (string, error) {
	netDir := buildSysBusPciPath(pciAddr, "net")
	netdevs, err := os.ReadDir(netDir)
	if err != nil {
		return "", fmt.Errorf("failed to read the netdevs of %s: %w", pciAddr, err)
	}

	guid := ""
	for _, netdev := range netdevs {
		linkType, err := os.ReadFile(filepath.Join(netDir, netdev.Name(), "type")) /* #nosec G304 */
		if err != nil || strings.TrimSpace(string(linkType)) != strconv.Itoa(ArphrdInfiniband) {
			continue
		}
		address, err := os.ReadFile(filepath.Join(netDir, netdev.Name(), "address")) /* #nosec G304 */
		if err != nil {
			continue
		}
		netdevGUID, err := guidFromHardwareAddress(string(address), ipoibAddressBytes)
		if err != nil {
			h.log.V(4).Info("ipoibPortGUID(): netdev address holds no port GUID",
				"device", pciAddr, "interface", netdev.Name(), "err", err)
			continue
		}
		if guid != "" && netdevGUID != guid {
			return "", fmt.Errorf("IPoIB netdevs of %s hold different port GUIDs: %w", pciAddr, ErrMultiplePorts)
		}
		guid = netdevGUID
	}
	if guid == "" {
		return "", fmt.Errorf("no IPoIB netdev of %s holds a port GUID", pciAddr)
	}
	return guid, nil
}

// GetVFIBPortGUIDs returns the port GUIDs set administratively on the VFs of
// an InfiniBand PF through its netdev (ip link set <pf> vf <n> port_guid
// ...), by VF ID. VFs without one are left out. The PF has to be known to
// have a single port, its RDMA device showing exactly one: on any other PF a
// VF GUID read through one netdev names one of several ports.
func (h *Host) GetVFIBPortGUIDs(pfPciAddr, pfName string) (map[int]string, error) {
	if pfName == "" {
		return nil, errors.New("no interface name to read the VF port GUIDs of")
	}
	if !pciAddressRegexp.MatchString(pfPciAddr) {
		return nil, fmt.Errorf("invalid PCI address %q", pfPciAddr)
	}
	if _, _, err := h.singleIBPort(pfPciAddr); err != nil {
		return nil, fmt.Errorf("PF %s is not known to have a single port: %w", pfPciAddr, err)
	}

	vfGUIDs, err := h.netlinkProvider.GetVfPortGUIDs(pfName)
	if err != nil {
		return nil, fmt.Errorf("failed to read the VF port GUIDs of %s: %w", pfName, err)
	}

	guids := make(map[int]string, len(vfGUIDs))
	for vfID, vfGUID := range vfGUIDs {
		if vfGUID == 0 {
			continue
		}
		guids[vfID] = fmt.Sprintf("%016x", vfGUID)
	}
	return guids, nil
}

// GetIBPortGUIDFromVPD derives the InfiniBand port GUID of a Mellanox/NVIDIA
// physical function from its PCI VPD, which stays readable without a kernel
// network driver (bound to vfio-pci), and returns it with the identity of
// that VPD. The rule (mellanoxPortGUIDFromVPD) is known to hold for
// single-port adapters only, so it is applied to nothing but function 0 of a
// slot that has no other physical function, on ConnectX-4 or later; any
// other function fails with ErrVPDNotApplicable. The caller has to rule out
// that two functions carry the VPD of one adapter.
//
// A VPD that yielded a GUID is not read again in this process: the device
// may be in use by a guest. A failed read is tried again on the next call.
func (h *Host) GetIBPortGUIDFromVPD(pciAddr string) (VPDPortGUID, error) {
	h.vpdMu.Lock()
	defer h.vpdMu.Unlock()

	if cached, ok := h.vpdPortGUIDs[pciAddr]; ok {
		return cached, nil
	}

	if err := h.vpdPortGUIDApplies(pciAddr); err != nil {
		return VPDPortGUID{}, err
	}

	data, err := readVPD(pciAddr)
	if err != nil {
		return VPDPortGUID{}, fmt.Errorf("failed to read the VPD of %s: %w", pciAddr, err)
	}
	portGUID, err := mellanoxPortGUIDFromVPD(data)
	if err != nil {
		return VPDPortGUID{}, fmt.Errorf("no port GUID in the VPD of %s: %w", pciAddr, err)
	}

	if h.vpdPortGUIDs == nil {
		h.vpdPortGUIDs = map[string]VPDPortGUID{}
	}
	h.vpdPortGUIDs[pciAddr] = portGUID
	h.log.V(2).Info("GetIBPortGUIDFromVPD(): read VPD", "device", pciAddr, "portGUID", portGUID.GUID)
	return portGUID, nil
}

// vpdPortGUIDApplies checks that the VPD rule may be applied to a function:
// a Mellanox/NVIDIA physical function of ConnectX-4 or later, function 0,
// alone in its slot but for its own VFs. A function that is known not to be
// one fails with ErrVPDNotApplicable, one that cannot be told with another
// error.
func (h *Host) vpdPortGUIDApplies(pciAddr string) error {
	if !pciAddressRegexp.MatchString(pciAddr) {
		return fmt.Errorf("invalid PCI address %q", pciAddr)
	}
	slot, function, _ := strings.Cut(pciAddr, ".")
	if function != "0" {
		return fmt.Errorf("%w: %s is not function 0", ErrVPDNotApplicable, pciAddr)
	}
	if h.IsSriovVF(pciAddr) {
		return fmt.Errorf("%w: %s is a virtual function", ErrVPDNotApplicable, pciAddr)
	}

	vendor, err := os.ReadFile(buildSysBusPciPath(pciAddr, "vendor")) /* #nosec G304 */
	if err != nil {
		return fmt.Errorf("failed to read the vendor of %s: %w", pciAddr, err)
	}
	vendorID := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(string(vendor))), "0x")
	if vendorID != consts.VendorIDMellanox {
		return fmt.Errorf("%w: %s is not a Mellanox/NVIDIA device (vendor %s)", ErrVPDNotApplicable, pciAddr, vendorID)
	}

	device, err := os.ReadFile(buildSysBusPciPath(pciAddr, "device")) /* #nosec G304 */
	if err != nil {
		return fmt.Errorf("failed to read the device ID of %s: %w", pciAddr, err)
	}
	deviceID, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(device)), "0x"), 16, 16)
	if err != nil {
		return fmt.Errorf("failed to parse the device ID of %s: %w", pciAddr, err)
	}
	if deviceID < mellanoxConnectX4DeviceID ||
		(deviceID >= mellanoxLegacyDeviceIDsFirst && deviceID <= mellanoxLegacyDeviceIDsLast) {
		return fmt.Errorf("%w: %s (device %04x) can carry two ports on one function",
			ErrVPDNotApplicable, pciAddr, deviceID)
	}

	functions, err := os.ReadDir(buildSysPath(consts.SysBusPci))
	if err != nil {
		return fmt.Errorf("failed to list PCI devices: %w", err)
	}
	for _, other := range functions {
		if other.Name() == pciAddr || !strings.HasPrefix(other.Name(), slot+".") {
			continue
		}
		if !h.IsSriovVF(other.Name()) {
			return fmt.Errorf("%w: %s shares its slot with physical function %s",
				ErrVPDNotApplicable, pciAddr, other.Name())
		}
	}
	return nil
}

// readVPD reads the VPD image of a PCI function from sysfs, bounded by the
// VPD address space.
func readVPD(pciAddr string) ([]byte, error) {
	vpd, err := os.Open(buildSysBusPciPath(pciAddr, "vpd")) /* #nosec G304 */
	if err != nil {
		return nil, err
	}
	defer vpd.Close()

	data, err := io.ReadAll(io.LimitReader(vpd, maxVPDSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxVPDSize {
		return nil, fmt.Errorf("VPD exceeds %d bytes", maxVPDSize)
	}
	return data, nil
}
