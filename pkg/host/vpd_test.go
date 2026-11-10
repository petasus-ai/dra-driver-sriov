package host

import (
	"bytes"
	"strings"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vishvananda/netlink/nl"
)

const (
	// A UUID whose last 12 digits are a MAC of the documentation range
	// (RFC 7042), and the port GUID that follows from it.
	testVPDUUID  = "0123456789abcdef800000005e0053a7"
	testVPDGUID  = "00005e03000053a7"
	testVPDAlias = "MLX:MN=MLNX:CSKU=V2:UUID=V3:PCI=V0:MODL=EX0001"
)

func vpdTestResource(tag byte, body []byte) []byte {
	return append([]byte{tag, byte(len(body)), byte(len(body) >> 8)}, body...)
}

func vpdTestField(keyword, value string) []byte {
	return append([]byte{keyword[0], keyword[1], byte(len(value))}, value...)
}

// vpdTestImageOf builds a VPD image: an identifier string, a VPD-R resource
// with the given body, and the end tag. With checksumAt >= 0 the byte of the
// body at that offset is set to close the checksum over the image up to it.
func vpdTestImageOf(body []byte, checksumAt int) []byte {
	image := vpdTestResource(vpdTagIdentifier, []byte("Example InfiniBand adapter, single-port"))
	bodyStart := len(image) + vpdLargeHeaderLen
	image = append(image, vpdTestResource(vpdTagReadOnly, body)...)

	if checksumAt >= 0 {
		image[bodyStart+checksumAt] = 0
		var sum byte
		for _, b := range image[:bodyStart+checksumAt] {
			sum += b
		}
		image[bodyStart+checksumAt] = -sum
	}
	return append(image, vpdTagEnd)
}

// vpdTestImage builds a VPD image whose VPD-R resource holds the given fields
// and is closed by an RV field with a correct checksum.
func vpdTestImage(fields ...[]byte) []byte {
	body := append(bytes.Join(fields, nil), 'R', 'V', 1, 0)
	return vpdTestImageOf(body, len(body)-1)
}

// NewTestVPD builds the VPD image of an adapter with the given UUID (V3) and
// alias list (VA), serial number EX0000000001 and a correct checksum; an
// empty value leaves its field out.
func NewTestVPD(uuid, alias string) []byte {
	fields := [][]byte{
		vpdTestField("PN", "EX-0001"),
		vpdTestField("EC", "A1"),
		vpdTestField("SN", "EX0000000001"),
		vpdTestField("V0", "PCIeGen5 x16"),
		vpdTestField("V2", "EX-0001"),
	}
	if uuid != "" {
		fields = append(fields, vpdTestField("V3", uuid))
	}
	if alias != "" {
		fields = append(fields, vpdTestField("VA", alias))
	}
	return vpdTestImage(fields...)
}

var _ = Describe("PCI VPD", func() {
	Context("parseVPDReadOnlyFields", func() {
		It("should return the keyword fields of the VPD-R resource", func() {
			fields, err := parseVPDReadOnlyFields(NewTestVPD(testVPDUUID, testVPDAlias))
			Expect(err).NotTo(HaveOccurred())

			keywords := []string{}
			for _, field := range fields {
				keywords = append(keywords, field.Keyword)
			}
			Expect(keywords).To(Equal([]string{"PN", "EC", "SN", "V0", "V2", "V3", "VA", "RV"}))
			Expect(string(fields[5].Value)).To(Equal(testVPDUUID))
			Expect(string(fields[6].Value)).To(Equal(testVPDAlias))
		})

		It("should carry a checksum that closes the image over VPD-R", func() {
			image := NewTestVPD(testVPDUUID, testVPDAlias)
			var sum byte
			for _, b := range image[:len(image)-1] {
				sum += b
			}
			Expect(sum).To(BeZero())
		})

		It("should skip a VPD-W resource", func() {
			image := NewTestVPD(testVPDUUID, "")
			image = image[:len(image)-1]
			image = append(image, vpdTestResource(vpdTagReadWrite, vpdTestField("V3", strings.Repeat("9", 32)))...)
			image = append(image, vpdTagEnd)

			fields, err := parseVPDReadOnlyFields(image)
			Expect(err).NotTo(HaveOccurred())
			Expect(fields).To(HaveLen(7))
		})

		It("should accept an end tag that carries a length", func() {
			image := NewTestVPD(testVPDUUID, "")
			image[len(image)-1] = vpdTagEnd | 1
			_, err := parseVPDReadOnlyFields(append(image, 0))
			Expect(err).NotTo(HaveOccurred())
		})

		It("should refuse an image whose bytes changed under its checksum", func() {
			image := NewTestVPD(testVPDUUID, testVPDAlias)
			image[bytes.Index(image, []byte(testVPDUUID))+31] ^= 0x01

			_, err := parseVPDReadOnlyFields(image)
			Expect(err).To(MatchError(ContainSubstring("VPD checksum fails")))
		})

		It("should refuse every single-bit change up to the checksum", func() {
			image := NewTestVPD(testVPDUUID, testVPDAlias)
			checksumAt := len(image) - 2
			for i := 0; i <= checksumAt; i++ {
				changed := append([]byte{}, image...)
				changed[i] ^= 0x04
				_, err := parseVPDReadOnlyFields(changed)
				Expect(err).To(HaveOccurred(), "byte %d of %d changed", i, len(image))
			}
		})

		It("should refuse a VPD-R resource without an RV field", func() {
			body := bytes.Join([][]byte{vpdTestField("SN", "EX0000000001"), vpdTestField("V3", testVPDUUID)}, nil)

			_, err := parseVPDReadOnlyFields(vpdTestImageOf(body, -1))
			Expect(err).To(MatchError(ContainSubstring("VPD-R has no RV field")))
		})

		It("should refuse an RV field that holds no checksum byte", func() {
			body := append(vpdTestField("V3", testVPDUUID), 'R', 'V', 0)

			_, err := parseVPDReadOnlyFields(vpdTestImageOf(body, -1))
			Expect(err).To(MatchError(ContainSubstring("RV field holds no checksum")))
		})

		It("should refuse fields the checksum does not cover", func() {
			body := append(vpdTestField("SN", "EX0000000001"), 'R', 'V', 1, 0)
			checksumAt := len(body) - 1
			body = append(body, vpdTestField("V3", testVPDUUID)...)

			_, err := parseVPDReadOnlyFields(vpdTestImageOf(body, checksumAt))
			Expect(err).To(MatchError(ContainSubstring("RV field does not close VPD-R")))
		})

		It("should accept an RV field with reserved bytes after the checksum", func() {
			body := append(vpdTestField("V3", testVPDUUID), 'R', 'V', 3, 0, 0xaa, 0x55)

			fields, err := parseVPDReadOnlyFields(vpdTestImageOf(body, len(body)-3))
			Expect(err).NotTo(HaveOccurred())
			Expect(fields).To(HaveLen(2))
		})

		DescribeTable("should refuse a malformed image",
			func(image []byte, message string) {
				_, err := parseVPDReadOnlyFields(image)
				Expect(err).To(MatchError(ContainSubstring(message)))
			},
			Entry("no data", []byte{}, "no end tag"),
			Entry("an unreadable device, all ones", bytes.Repeat([]byte{0xff}, 64), "unexpected VPD large resource tag 0xff"),
			Entry("an empty EEPROM, all zeros", make([]byte, 64), "unexpected VPD small resource tag 0x00"),
			Entry("only an end tag", []byte{vpdTagEnd}, "no VPD-R resource"),
			Entry("an identifier string alone",
				append(vpdTestResource(vpdTagIdentifier, []byte("Example")), vpdTagEnd), "no VPD-R resource"),
			Entry("a resource that is no VPD resource",
				append(vpdTestResource(0x84, []byte("data")), vpdTagEnd), "unexpected VPD large resource tag 0x84"),
			Entry("a resource longer than the data",
				[]byte{vpdTagReadOnly, 0x40, 0x00, 'P', 'N', 1, 'x', vpdTagEnd}, "exceeds the data"),
			Entry("a field longer than its resource",
				append(vpdTestResource(vpdTagReadOnly, []byte{'P', 'N', 9, 'x'}), vpdTagEnd), "exceeds its resource"),
			Entry("a field header cut short",
				append(vpdTestResource(vpdTagReadOnly, []byte{'P', 'N'}), vpdTagEnd), "truncated VPD field header"),
			Entry("a keyword that is no keyword",
				append(vpdTestResource(vpdTagReadOnly, []byte{'p', 0x00, 1, 'x'}), vpdTagEnd), "invalid VPD keyword"),
			Entry("two VPD-R resources",
				append(append(vpdTestResource(vpdTagReadOnly, vpdTestField("PN", "a")),
					vpdTestResource(vpdTagReadOnly, vpdTestField("PN", "b"))...), vpdTagEnd),
				"more than one VPD-R"),
		)
	})

	Context("mellanoxPortGUIDFromVPD", func() {
		It("should put 03:00 in the middle of the base MAC the UUID ends with", func() {
			portGUID, err := mellanoxPortGUIDFromVPD(NewTestVPD(testVPDUUID, testVPDAlias))
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID.GUID).To(Equal(testVPDGUID))
		})

		It("should return the serial number and UUID that identify the VPD", func() {
			portGUID, err := mellanoxPortGUIDFromVPD(NewTestVPD(testVPDUUID, testVPDAlias))
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID).To(Equal(VPDPortGUID{GUID: testVPDGUID, SerialNumber: "EX0000000001", UUID: testVPDUUID}))
		})

		It("should return a padded serial number without its padding", func() {
			image := vpdTestImage(vpdTestField("SN", "EX0000000002   "), vpdTestField("V3", testVPDUUID))

			portGUID, err := mellanoxPortGUIDFromVPD(image)
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID.SerialNumber).To(Equal("EX0000000002"))
		})

		It("should return an empty serial number for a VPD without one", func() {
			portGUID, err := mellanoxPortGUIDFromVPD(vpdTestImage(vpdTestField("V3", testVPDUUID)))
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID).To(Equal(VPDPortGUID{GUID: testVPDGUID, UUID: testVPDUUID}))
		})

		It("should accept a UUID without a VA field", func() {
			portGUID, err := mellanoxPortGUIDFromVPD(NewTestVPD(testVPDUUID, ""))
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID.GUID).To(Equal(testVPDGUID))
		})

		It("should render an upper-case UUID in lower case", func() {
			portGUID, err := mellanoxPortGUIDFromVPD(NewTestVPD(strings.ToUpper(testVPDUUID), testVPDAlias))
			Expect(err).NotTo(HaveOccurred())
			Expect(portGUID.GUID).To(Equal(testVPDGUID))
			Expect(portGUID.UUID).To(Equal(testVPDUUID))
		})

		It("should refuse a UUID digit that changed under the checksum", func() {
			image := NewTestVPD(testVPDUUID, testVPDAlias)
			image[bytes.Index(image, []byte(testVPDUUID))+31] ^= 0x01

			portGUID, err := mellanoxPortGUIDFromVPD(image)
			Expect(err).To(MatchError(ContainSubstring("VPD checksum fails")))
			Expect(portGUID).To(Equal(VPDPortGUID{}))
		})

		It("should not read the UUID from VPD-W", func() {
			image := NewTestVPD("", "")
			image = image[:len(image)-1]
			image = append(image, vpdTestResource(vpdTagReadWrite, vpdTestField("V3", testVPDUUID))...)
			image = append(image, vpdTagEnd)

			_, err := mellanoxPortGUIDFromVPD(image)
			Expect(err).To(MatchError(ContainSubstring("no V3 field")))
		})

		It("should refuse every truncation of an image", func() {
			image := NewTestVPD(testVPDUUID, testVPDAlias)
			for size := 0; size < len(image); size++ {
				portGUID, err := mellanoxPortGUIDFromVPD(image[:size])
				Expect(err).To(HaveOccurred(), "image cut to %d of %d bytes", size, len(image))
				Expect(portGUID).To(Equal(VPDPortGUID{}))
			}
		})

		DescribeTable("should refuse a UUID it cannot trust",
			func(image []byte, message string) {
				portGUID, err := mellanoxPortGUIDFromVPD(image)
				Expect(err).To(MatchError(ContainSubstring(message)))
				Expect(portGUID).To(Equal(VPDPortGUID{}))
			},
			Entry("no V3 field", NewTestVPD("", testVPDAlias), "no V3 field"),
			Entry("a VA field that names another keyword as the UUID",
				NewTestVPD(testVPDUUID, "MLX:MN=MLNX:CSKU=V2:UUID=V1:PCI=V0"), "does not name UUID=V3"),
			Entry("a VA field that names no UUID",
				NewTestVPD(testVPDUUID, "MLX:MN=MLNX:CSKU=V2:PCI=V0"), "does not name UUID=V3"),
			Entry("a VA field that only resembles the name",
				NewTestVPD(testVPDUUID, "MLX:MN=MLNX:XUUID=V3:UUID=V30"), "does not name UUID=V3"),
			Entry("a UUID one digit short", NewTestVPD(testVPDUUID[1:], testVPDAlias), "not a UUID of 32 hex digits"),
			Entry("a UUID one digit long", NewTestVPD(testVPDUUID+"0", testVPDAlias), "not a UUID of 32 hex digits"),
			Entry("a UUID with separators",
				NewTestVPD("01234567-89ab-cdef-8000-00005e0053a7", testVPDAlias), "not a UUID of 32 hex digits"),
			Entry("a UUID with a digit that is no hex digit",
				NewTestVPD("g"+testVPDUUID[1:], testVPDAlias), "not a UUID of 32 hex digits"),
			Entry("a padded UUID", NewTestVPD(testVPDUUID[:31]+" ", testVPDAlias), "not a UUID of 32 hex digits"),
			Entry("a UUID that ends in an all-zero MAC",
				NewTestVPD(testVPDUUID[:20]+"000000000000", testVPDAlias), "holds no base MAC"),
			Entry("a UUID that ends in an all-ones MAC",
				NewTestVPD(testVPDUUID[:20]+"FFFFFFFFFFFF", testVPDAlias), "holds no base MAC"),
			Entry("two V3 fields",
				vpdTestImage(vpdTestField("V3", testVPDUUID), vpdTestField("V3", testVPDUUID)), "more than one V3"),
			Entry("two SN fields",
				vpdTestImage(vpdTestField("SN", "EX0000000001"), vpdTestField("SN", "EX0000000002"),
					vpdTestField("V3", testVPDUUID)), "more than one SN"),
			Entry("two VA fields",
				vpdTestImage(vpdTestField("V3", testVPDUUID), vpdTestField("VA", testVPDAlias),
					vpdTestField("VA", testVPDAlias)), "more than one VA"),
		)
	})

	Context("normalizeGUID", func() {
		DescribeTable("should render a GUID as 16 lowercase hex digits",
			func(guid string) {
				normalized, err := normalizeGUID(guid)
				Expect(err).NotTo(HaveOccurred())
				Expect(normalized).To(Equal(testVPDGUID))
			},
			Entry("bare digits", "00005e03000053a7"),
			Entry("upper case", "00005E03000053A7"),
			Entry("bytes with colons", "00:00:5e:03:00:00:53:a7"),
			Entry("bytes with dashes", "00-00-5E-03-00-00-53-A7"),
			Entry("16-bit groups, as in a GID", "0000:5e03:0000:53a7"),
			Entry("a 0x prefix", "0x00005e03000053a7"),
			Entry("surrounding white space", " 00005e03000053a7\n"),
		)

		DescribeTable("should refuse what is no GUID",
			func(guid string) {
				normalized, err := normalizeGUID(guid)
				Expect(err).To(HaveOccurred())
				Expect(normalized).To(BeEmpty())
			},
			Entry("nothing", ""),
			Entry("all zeros", "0000000000000000"),
			Entry("all zeros with separators", "00:00:00:00:00:00:00:00"),
			Entry("15 digits", "00005e03000053a"),
			Entry("17 digits", "00005e03000053a70"),
			Entry("a digit that is no hex digit", "00005e03000053ag"),
			Entry("a MAC", "00:00:5e:00:53:a7"),
		)
	})

	Context("guidFromHardwareAddress", func() {
		It("should take the last 8 bytes of a GID", func() {
			guid, err := guidFromHardwareAddress("fe80:0000:0000:0000:0000:5e03:0000:53a7\n", gidBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(guid).To(Equal(testVPDGUID))
		})

		It("should take the last 8 bytes of an IPoIB address", func() {
			guid, err := guidFromHardwareAddress(
				"00:00:10:49:fe:80:00:00:00:00:00:00:00:00:5e:03:00:00:53:a7\n", ipoibAddressBytes)
			Expect(err).NotTo(HaveOccurred())
			Expect(guid).To(Equal(testVPDGUID))
		})

		DescribeTable("should refuse an address that holds no GUID",
			func(address string, wantBytes int) {
				guid, err := guidFromHardwareAddress(address, wantBytes)
				Expect(err).To(HaveOccurred())
				Expect(guid).To(BeEmpty())
			},
			Entry("an Ethernet address where an IPoIB address is due", "00:00:5e:00:53:a7", ipoibAddressBytes),
			Entry("an IPoIB address where a GID is due",
				"00:00:10:49:fe:80:00:00:00:00:00:00:00:00:5e:03:00:00:53:a7", gidBytes),
			Entry("a GID without a GUID", "fe80:0000:0000:0000:0000:0000:0000:0000", gidBytes),
			Entry("an IPoIB address without a GUID",
				"00:00:10:49:fe:80:00:00:00:00:00:00:00:00:00:00:00:00:00:00", ipoibAddressBytes),
			Entry("no address", "", gidBytes),
			Entry("an address that is no hex", "fe80:0000:0000:0000:0000:5e03:0000:53zz", gidBytes),
		)
	})
})

var _ = Describe("parseVfPortGUIDs", func() {
	vfInfo := func(list *nl.RtAttr, vf uint32, portGUID uint64) {
		info := list.AddRtAttr(nl.IFLA_VF_INFO, nil)
		mac := nl.VfMac{Vf: vf}
		info.AddRtAttr(nl.IFLA_VF_MAC, mac.Serialize())
		nodeGUID := nl.VfGUID{Vf: vf, GUID: 0x00005e03000053ff}
		info.AddRtAttr(nl.IFLA_VF_IB_NODE_GUID, nodeGUID.Serialize())
		guid := nl.VfGUID{Vf: vf, GUID: portGUID}
		info.AddRtAttr(nl.IFLA_VF_IB_PORT_GUID, guid.Serialize())
	}

	linkMessage := func(attrs ...*nl.RtAttr) []byte {
		msg := nl.NewIfInfomsg(syscall.AF_UNSPEC).Serialize()
		msg = append(msg, nl.NewRtAttr(syscall.IFLA_IFNAME, nl.ZeroTerminated("ib0")).Serialize()...)
		for _, attr := range attrs {
			msg = append(msg, attr.Serialize()...)
		}
		return msg
	}

	It("should read the port GUID of each VF by its index", func() {
		list := nl.NewRtAttr(iflaVfinfoList, nil)
		vfInfo(list, 0, 0x00005e03000053a7)
		vfInfo(list, 1, 0)
		vfInfo(list, 5, 0x00005e03000053a8)

		guids, err := parseVfPortGUIDs(linkMessage(list))
		Expect(err).NotTo(HaveOccurred())
		Expect(guids).To(Equal(map[int]uint64{0: 0x00005e03000053a7, 1: 0, 5: 0x00005e03000053a8}))
	})

	It("should return none for a link without VFs", func() {
		guids, err := parseVfPortGUIDs(linkMessage())
		Expect(err).NotTo(HaveOccurred())
		Expect(guids).To(BeEmpty())
	})

	It("should return none for VFs that carry no InfiniBand GUID", func() {
		list := nl.NewRtAttr(iflaVfinfoList, nil)
		mac := nl.VfMac{Vf: 0}
		list.AddRtAttr(nl.IFLA_VF_INFO, nil).AddRtAttr(nl.IFLA_VF_MAC, mac.Serialize())

		guids, err := parseVfPortGUIDs(linkMessage(list))
		Expect(err).NotTo(HaveOccurred())
		Expect(guids).To(BeEmpty())
	})

	It("should refuse a truncated port GUID", func() {
		list := nl.NewRtAttr(iflaVfinfoList, nil)
		list.AddRtAttr(nl.IFLA_VF_INFO, nil).AddRtAttr(nl.IFLA_VF_IB_PORT_GUID, []byte{0, 0, 0, 0, 1, 2, 3, 4})

		_, err := parseVfPortGUIDs(linkMessage(list))
		Expect(err).To(MatchError(ContainSubstring("truncated VF port GUID")))
	})

	It("should refuse a message shorter than its header", func() {
		_, err := parseVfPortGUIDs([]byte{0, 0, 0})
		Expect(err).To(MatchError(ContainSubstring("truncated link message")))
	})
})
