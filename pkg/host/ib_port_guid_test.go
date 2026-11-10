package host_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"

	"github.com/k8snetworkplumbingwg/dra-driver-sriov/pkg/host"
	mock_host "github.com/k8snetworkplumbingwg/dra-driver-sriov/pkg/host/mock"
)

var _ = Describe("InfiniBand Port GUID Functions", func() {
	const (
		pfAddr = "0000:19:00.0"
		pfDir  = "sys/bus/pci/devices/0000:19:00.0"

		// A UUID whose last 12 digits are a MAC of the documentation range
		// (RFC 7042), and the port GUID that follows from it.
		vpdUUID = "0123456789abcdef800000005e0053a7"
		vpdGUID = "00005e03000053a7"

		rdmaGID      = "fe80:0000:0000:0000:0000:5e03:0000:53b1\n"
		rdmaGUID     = "00005e03000053b1"
		ipoibAddress = "00:00:10:49:fe:80:00:00:00:00:00:00:00:00:5e:03:00:00:53:c2\n"
		ipoibGUID    = "00005e03000053c2"
		typeIPoIB    = "32\n"
		typeEthernet = "1\n"
	)

	var (
		fs       *host.FakeFilesystem
		tearDown func()
	)

	BeforeEach(func() {
		fs = &host.FakeFilesystem{Files: map[string][]byte{}, Symlinks: map[string]string{}}
		tearDown = nil
	})

	AfterEach(func() {
		if tearDown != nil {
			tearDown()
		}
	})

	Context("GetIBPortGUID", func() {
		var (
			mockCtrl         *gomock.Controller
			mockRdmaProvider *mock_host.MockRdmaProvider
			h                *host.Host
		)

		BeforeEach(func() {
			mockCtrl = gomock.NewController(GinkgoT())
			mockRdmaProvider = mock_host.NewMockRdmaProvider(mockCtrl)
			h = host.NewHost().(*host.Host)
			h.SetRdmaProvider(mockRdmaProvider)
		})

		AfterEach(func() {
			mockCtrl.Finish()
		})

		rdmaDevice := func(name, linkLayer, gid string, ports ...string) {
			for _, port := range ports {
				portDir := filepath.Join("sys/class/infiniband", name, "ports", port)
				fs.Dirs = append(fs.Dirs, filepath.Join(portDir, "gids"))
				fs.Files[filepath.Join(portDir, "link_layer")] = []byte(linkLayer + "\n")
				fs.Files[filepath.Join(portDir, "gids", "0")] = []byte(gid)
			}
		}

		netdev := func(name, linkType, address string) {
			netdevDir := filepath.Join(pfDir, "net", name)
			fs.Dirs = append(fs.Dirs, netdevDir)
			fs.Files[filepath.Join(netdevDir, "type")] = []byte(linkType)
			fs.Files[filepath.Join(netdevDir, "address")] = []byte(address)
		}

		It("should read the GUID from GID 0 of the RDMA device", func() {
			rdmaDevice("mlx5_0", "InfiniBand", rdmaGID, "1")
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return([]string{"mlx5_0"})

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(guid).To(Equal(rdmaGUID))
		})

		It("should prefer the RDMA device over the IPoIB netdev", func() {
			rdmaDevice("mlx5_0", "InfiniBand", rdmaGID, "1")
			netdev("ib0", typeIPoIB, ipoibAddress)
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return([]string{"mlx5_0"})

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(guid).To(Equal(rdmaGUID))
		})

		It("should read the GUID from the IPoIB netdev when there is no RDMA device", func() {
			netdev("ib0", typeIPoIB, ipoibAddress)
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return(nil)

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(guid).To(Equal(ipoibGUID))
		})

		It("should read the GUID from the IPoIB netdev when GID 0 holds none", func() {
			rdmaDevice("mlx5_0", "InfiniBand", "fe80:0000:0000:0000:0000:0000:0000:0000\n", "1")
			netdev("ib0", typeIPoIB, ipoibAddress)
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return([]string{"mlx5_0"})

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(guid).To(Equal(ipoibGUID))
		})

		It("should take the GUID the netdev and its P_Key children share", func() {
			netdev("ib0", typeIPoIB, ipoibAddress)
			netdev("ib0.8001", typeIPoIB, "00:00:10:4b:fe:80:00:00:00:00:00:00:00:00:5e:03:00:00:53:c2\n")
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return(nil)

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(guid).To(Equal(ipoibGUID))
		})

		It("should not report an RDMA device with more than one port", func() {
			rdmaDevice("mlx4_0", "InfiniBand", rdmaGID, "1", "2")
			netdev("ib0", typeIPoIB, ipoibAddress)
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return([]string{"mlx4_0"})

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).To(MatchError(host.ErrMultiplePorts))
			Expect(guid).To(BeEmpty())
		})

		It("should not report a function with more than one RDMA device", func() {
			rdmaDevice("mlx5_0", "InfiniBand", rdmaGID, "1")
			rdmaDevice("mlx5_1", "InfiniBand", rdmaGID, "1")
			netdev("ib0", typeIPoIB, ipoibAddress)
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return([]string{"mlx5_0", "mlx5_1"})

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).To(MatchError(host.ErrMultiplePorts))
			Expect(guid).To(BeEmpty())
		})

		It("should not report netdevs that hold different GUIDs", func() {
			netdev("ib0", typeIPoIB, ipoibAddress)
			netdev("ib1", typeIPoIB, "00:00:10:49:fe:80:00:00:00:00:00:00:00:00:5e:03:00:00:53:c3\n")
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return(nil)

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).To(MatchError(host.ErrMultiplePorts))
			Expect(guid).To(BeEmpty())
		})

		It("should not take a RoCE GID or an Ethernet address for a GUID", func() {
			rdmaDevice("mlx5_0", "Ethernet", "fe80:0000:0000:0000:0200:5eff:fe00:53a7\n", "1")
			netdev("eth0", typeEthernet, "00:00:5e:00:53:a7\n")
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return([]string{"mlx5_0"})

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).To(MatchError(ContainSubstring("link layer Ethernet")))
			Expect(guid).To(BeEmpty())
		})

		It("should know none for a function without a kernel network driver", func() {
			fs.Dirs = []string{pfDir}
			tearDown = fs.Use()
			mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return(nil)

			guid, err := h.GetIBPortGUID(pfAddr)
			Expect(err).To(MatchError(ContainSubstring("no kernel port GUID")))
			Expect(err).NotTo(MatchError(host.ErrMultiplePorts))
			Expect(guid).To(BeEmpty())
		})

		It("should refuse what is no PCI address", func() {
			tearDown = fs.Use()

			_, err := h.GetIBPortGUID("../0000:19:00.0")
			Expect(err).To(MatchError(ContainSubstring("invalid PCI address")))
		})
	})

	Context("GetVFIBPortGUIDs", func() {
		var (
			mockCtrl         *gomock.Controller
			mockRdmaProvider *mock_host.MockRdmaProvider
		)

		BeforeEach(func() {
			mockCtrl = gomock.NewController(GinkgoT())
			mockRdmaProvider = mock_host.NewMockRdmaProvider(mockCtrl)
		})

		AfterEach(func() {
			mockCtrl.Finish()
		})

		// pfHost returns a host whose PF has an RDMA device with the given
		// ports, or none without ports.
		pfHost := func(netlinkProvider *host.FakeNetlinkProvider, ports ...string) host.Interface {
			for _, port := range ports {
				portDir := filepath.Join("sys/class/infiniband/mlx5_0/ports", port)
				fs.Dirs = append(fs.Dirs, portDir)
				fs.Files[filepath.Join(portDir, "link_layer")] = []byte("InfiniBand\n")
			}
			tearDown = fs.Use()
			h := host.NewHostForTest(netlinkProvider).(*host.Host)
			h.SetRdmaProvider(mockRdmaProvider)
			if len(ports) > 0 {
				mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return([]string{"mlx5_0"})
			} else {
				mockRdmaProvider.EXPECT().GetRdmaDevicesForPcidev(pfAddr).Return(nil)
			}
			return h
		}

		vfGUIDs := &host.FakeNetlinkProvider{
			VfPortGUIDs: map[string]map[int]uint64{
				"ib0": {0: 0x00005e03000053a7, 1: 0, 2: 0xfe005e03000053a8},
			},
		}

		It("should render the GUIDs set on the VFs and leave out the VFs without one", func() {
			h := pfHost(vfGUIDs, "1")

			guids, err := h.GetVFIBPortGUIDs(pfAddr, "ib0")
			Expect(err).NotTo(HaveOccurred())
			Expect(guids).To(Equal(map[int]string{0: "00005e03000053a7", 2: "fe005e03000053a8"}))
		})

		It("should return none for a PF that reports no VF GUIDs", func() {
			h := pfHost(&host.FakeNetlinkProvider{}, "1")

			guids, err := h.GetVFIBPortGUIDs(pfAddr, "ib0")
			Expect(err).NotTo(HaveOccurred())
			Expect(guids).To(BeEmpty())
		})

		It("should not ask a PF whose RDMA device has more than one port", func() {
			h := pfHost(vfGUIDs, "1", "2")

			guids, err := h.GetVFIBPortGUIDs(pfAddr, "ib0")
			Expect(err).To(MatchError(host.ErrMultiplePorts))
			Expect(guids).To(BeEmpty())
		})

		It("should not ask a PF without an RDMA device to count the ports of", func() {
			h := pfHost(vfGUIDs)

			guids, err := h.GetVFIBPortGUIDs(pfAddr, "ib0")
			Expect(err).To(MatchError(ContainSubstring("not known to have a single port")))
			Expect(guids).To(BeEmpty())
		})

		It("should return the netlink error", func() {
			h := pfHost(&host.FakeNetlinkProvider{VfPortGUIDsError: fmt.Errorf("no such device")}, "1")

			_, err := h.GetVFIBPortGUIDs(pfAddr, "ib0")
			Expect(err).To(MatchError(ContainSubstring("no such device")))
		})

		It("should refuse an empty interface name", func() {
			h := host.NewHostForTest(&host.FakeNetlinkProvider{})

			_, err := h.GetVFIBPortGUIDs(pfAddr, "")
			Expect(err).To(HaveOccurred())
		})
	})

	Context("GetIBPortGUIDFromVPD", func() {
		const (
			vpdSerial = "EX0000000001"
			vpdAlias  = "MLX:MN=MLNX:CSKU=V2:UUID=V3:PCI=V0"
		)

		var h host.Interface

		BeforeEach(func() {
			h = host.NewHostForTest(&host.FakeNetlinkProvider{})
		})

		function := func(address, vendor string) string {
			dir := filepath.Join("sys/bus/pci/devices", address)
			fs.Dirs = append(fs.Dirs, dir)
			fs.Files[filepath.Join(dir, "vendor")] = []byte(vendor + "\n")
			fs.Files[filepath.Join(dir, "device")] = []byte("0x1021\n")
			return dir
		}

		It("should derive the GUID from the VPD of a single-port PF, with the identity of the VPD", func() {
			dir := function(pfAddr, "0x15b3")
			fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, vpdAlias)
			tearDown = fs.Use()

			portGUID, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID).To(Equal(host.VPDPortGUID{GUID: vpdGUID, SerialNumber: vpdSerial, UUID: vpdUUID}))
		})

		It("should not read a VPD again that yielded a GUID", func() {
			dir := function(pfAddr, "0x15b3")
			fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
			tearDown = fs.Use()

			portGUID, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID.GUID).To(Equal(vpdGUID))

			Expect(os.Remove(filepath.Join(fs.RootDir, dir, "vpd"))).To(Succeed())

			portGUID, err = h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID.GUID).To(Equal(vpdGUID))
		})

		It("should read the VPD again after a failed read", func() {
			dir := function(pfAddr, "0x15b3")
			fs.Files[filepath.Join(dir, "vpd")] = []byte{0xff, 0xff, 0xff, 0xff}
			tearDown = fs.Use()

			portGUID, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).To(MatchError(ContainSubstring("no port GUID in the VPD")))
			Expect(err).NotTo(MatchError(host.ErrVPDNotApplicable))
			Expect(portGUID).To(Equal(host.VPDPortGUID{}))

			vpdPath := filepath.Join(fs.RootDir, dir, "vpd")
			Expect(os.WriteFile(vpdPath, host.NewTestVPD(vpdUUID, ""), 0o600)).To(Succeed())

			portGUID, err = h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID.GUID).To(Equal(vpdGUID))
		})

		It("should fail for a function that has no readable VPD", func() {
			function(pfAddr, "0x15b3")
			tearDown = fs.Use()

			_, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).To(MatchError(ContainSubstring("failed to read the VPD")))
			Expect(err).NotTo(MatchError(host.ErrVPDNotApplicable))
		})

		It("should refuse a VPD whose checksum fails", func() {
			dir := function(pfAddr, "0x15b3")
			vpd := host.NewTestVPD(vpdUUID, vpdAlias)
			vpd[bytes.Index(vpd, []byte(vpdUUID))+31] ^= 0x01
			fs.Files[filepath.Join(dir, "vpd")] = vpd
			tearDown = fs.Use()

			_, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).To(MatchError(ContainSubstring("VPD checksum fails")))
		})

		It("should refuse a VPD larger than the VPD address space", func() {
			dir := function(pfAddr, "0x15b3")
			fs.Files[filepath.Join(dir, "vpd")] = append(host.NewTestVPD(vpdUUID, ""), make([]byte, 1<<15)...)
			tearDown = fs.Use()

			_, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).To(MatchError(ContainSubstring("VPD exceeds")))
		})

		It("should keep the VFs of the PF out of the count of functions in its slot", func() {
			dir := function(pfAddr, "0x15b3")
			fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
			function("0000:19:00.1", "0x15b3")
			fs.Symlinks["sys/bus/pci/devices/0000:19:00.1/physfn"] = "../0000:19:00.0"
			tearDown = fs.Use()

			portGUID, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID.GUID).To(Equal(vpdGUID))
		})

		// The functions below carry a VPD that would yield a GUID: the rule
		// is not known to hold for them, so it must not be applied.
		It("should not apply the rule to a PF that shares its slot with another PF", func() {
			dir := function(pfAddr, "0x15b3")
			fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
			function("0000:19:00.1", "0x15b3")
			tearDown = fs.Use()

			portGUID, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).To(MatchError(host.ErrVPDNotApplicable))
			Expect(err).To(MatchError(ContainSubstring("shares its slot with physical function 0000:19:00.1")))
			Expect(portGUID).To(Equal(host.VPDPortGUID{}))
		})

		It("should not apply the rule to a function other than 0", func() {
			function(pfAddr, "0x15b3")
			dir := function("0000:19:00.1", "0x15b3")
			fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
			tearDown = fs.Use()

			_, err := h.GetIBPortGUIDFromVPD("0000:19:00.1")
			Expect(err).To(MatchError(host.ErrVPDNotApplicable))
			Expect(err).To(MatchError(ContainSubstring("not function 0")))
		})

		It("should not apply the rule to a virtual function", func() {
			function("0000:19:00.0", "0x15b3")
			dir := function("0000:19:01.0", "0x15b3")
			fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
			fs.Symlinks["sys/bus/pci/devices/0000:19:01.0/physfn"] = "../0000:19:00.0"
			tearDown = fs.Use()

			_, err := h.GetIBPortGUIDFromVPD("0000:19:01.0")
			Expect(err).To(MatchError(host.ErrVPDNotApplicable))
			Expect(err).To(MatchError(ContainSubstring("is a virtual function")))
		})

		It("should not apply the rule to another vendor's device", func() {
			dir := function(pfAddr, "0x8086")
			fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
			tearDown = fs.Use()

			_, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).To(MatchError(host.ErrVPDNotApplicable))
			Expect(err).To(MatchError(ContainSubstring("not a Mellanox/NVIDIA device")))
		})

		DescribeTable("should not apply the rule to an adapter that can carry two ports on one function",
			func(deviceID string) {
				dir := function(pfAddr, "0x15b3")
				fs.Files[filepath.Join(dir, "device")] = []byte(deviceID + "\n")
				fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
				tearDown = fs.Use()

				portGUID, err := h.GetIBPortGUIDFromVPD(pfAddr)
				Expect(err).To(MatchError(host.ErrVPDNotApplicable))
				Expect(err).To(MatchError(ContainSubstring("can carry two ports on one function")))
				Expect(portGUID).To(Equal(host.VPDPortGUID{}))
			},
			Entry("ConnectX-3", "0x1003"),
			Entry("ConnectX-3 Pro", "0x1007"),
			Entry("the last mlx4 ID", "0x1010"),
			Entry("Connect-IB", "0x1011"),
			Entry("Connect-IB virtual function", "0x1012"),
			Entry("ConnectX-2", "0x676e"),
			Entry("InfiniHost", "0x5a44"),
		)

		DescribeTable("should apply the rule to ConnectX-4 and later",
			func(deviceID string) {
				dir := function(pfAddr, "0x15b3")
				fs.Files[filepath.Join(dir, "device")] = []byte(deviceID + "\n")
				fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
				tearDown = fs.Use()

				portGUID, err := h.GetIBPortGUIDFromVPD(pfAddr)
				Expect(err).NotTo(HaveOccurred())
				Expect(portGUID.GUID).To(Equal(vpdGUID))
			},
			Entry("ConnectX-4", "0x1013"),
			Entry("ConnectX-6", "0x101b"),
			Entry("ConnectX-7", "0x1021"),
			Entry("ConnectX-8", "0x1023"),
		)

		It("should fail for a function whose device ID cannot be read", func() {
			dir := function(pfAddr, "0x15b3")
			fs.Files[filepath.Join(dir, "device")] = []byte("unknown\n")
			fs.Files[filepath.Join(dir, "vpd")] = host.NewTestVPD(vpdUUID, "")
			tearDown = fs.Use()

			_, err := h.GetIBPortGUIDFromVPD(pfAddr)
			Expect(err).To(MatchError(ContainSubstring("failed to parse the device ID")))
			Expect(err).NotTo(MatchError(host.ErrVPDNotApplicable))
		})

		It("should refuse what is no PCI address", func() {
			tearDown = fs.Use()

			_, err := h.GetIBPortGUIDFromVPD("0000:19:00")
			Expect(err).To(MatchError(ContainSubstring("invalid PCI address")))
		})
	})
})
