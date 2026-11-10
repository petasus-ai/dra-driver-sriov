package host

import (
	"errors"
	"fmt"
	"strings"
)

// PCI Vital Product Data, PCI Local Bus Specification 3.0 appendix I: a
// sequence of resources, each led by a tag byte. A large resource (bit 7 set)
// carries a 16-bit little-endian length, a small one its length in the low
// three bits of the tag.
const (
	vpdTagIdentifier = 0x82 // large resource: identifier string
	vpdTagReadOnly   = 0x90 // large resource: VPD-R keyword fields
	vpdTagReadWrite  = 0x91 // large resource: VPD-W keyword fields
	vpdTagEnd        = 0x78 // small resource: end tag

	vpdLargeResource  = 0x80
	vpdSmallTagMask   = 0x78
	vpdLargeHeaderLen = 3
	vpdFieldHeaderLen = 3

	// maxVPDSize is the VPD address space of a PCI function (15 bits).
	maxVPDSize = 1 << 15

	// VPD-R keywords: SN is the serial number, RV closes VPD-R with a
	// checksum byte.
	vpdKeywordSerialNumber = "SN"
	vpdKeywordChecksum     = "RV"

	// Mellanox/NVIDIA vendor-specific VPD-R keywords: V3 holds the adapter
	// UUID, VA a colon-separated list that names the keyword of each item.
	vpdKeywordMellanoxUUID  = "V3"
	vpdKeywordMellanoxAlias = "VA"
	vpdMellanoxUUIDAlias    = "UUID=" + vpdKeywordMellanoxUUID

	uuidHexLen = 32
	macHexLen  = 12
	guidHexLen = 16
)

// VPDPortGUID is a port GUID derived from the VPD of a PCI function, with
// what identifies that VPD: two functions that report the same serial number
// or UUID carry the VPD of one adapter.
type VPDPortGUID struct {
	GUID string
	// SerialNumber is the SN keyword; empty when the VPD has none.
	SerialNumber string
	// UUID is the adapter UUID the GUID is derived from, in lower case.
	UUID string
}

// vpdField is one keyword field of a VPD-R or VPD-W resource.
type vpdField struct {
	Keyword string
	Value   []byte
	// offset of Value in the resource body.
	offset int
}

// parseVPDReadOnlyFields returns the keyword fields of the VPD-R resource of
// a VPD image. The image has to be whole: every resource and field within the
// data, nothing but VPD resources, one VPD-R, and an end tag. VPD-R has to
// end with an RV field whose first byte brings the sum of all bytes of the
// image up to and including it to 0 modulo 256.
func parseVPDReadOnlyFields(data []byte) ([]vpdField, error) {
	var fields []vpdField
	sawReadOnly := false
	readOnlyStart := 0

	for off := 0; ; {
		if off >= len(data) {
			return nil, errors.New("VPD has no end tag")
		}
		tag := data[off]
		if tag&vpdLargeResource == 0 {
			if tag&vpdSmallTagMask != vpdTagEnd {
				return nil, fmt.Errorf("unexpected VPD small resource tag 0x%02x at offset %d", tag, off)
			}
			break
		}

		if tag != vpdTagIdentifier && tag != vpdTagReadOnly && tag != vpdTagReadWrite {
			return nil, fmt.Errorf("unexpected VPD large resource tag 0x%02x at offset %d", tag, off)
		}
		if off+vpdLargeHeaderLen > len(data) {
			return nil, fmt.Errorf("truncated VPD resource header at offset %d", off)
		}
		start := off + vpdLargeHeaderLen
		end := start + (int(data[off+1]) | int(data[off+2])<<8)
		if end > len(data) {
			return nil, fmt.Errorf("VPD resource 0x%02x at offset %d exceeds the data", tag, off)
		}

		if tag == vpdTagReadOnly {
			if sawReadOnly {
				return nil, errors.New("VPD has more than one VPD-R resource")
			}
			sawReadOnly = true
			readOnlyStart = start
			var err error
			fields, err = parseVPDFields(data[start:end])
			if err != nil {
				return nil, err
			}
		}
		off = end
	}

	if !sawReadOnly {
		return nil, errors.New("VPD has no VPD-R resource")
	}
	if err := verifyVPDChecksum(data, readOnlyStart, fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// verifyVPDChecksum checks the RV field of a VPD-R resource whose body starts
// at readOnlyStart: the last field, and the only RV, with the checksum in
// its first byte.
func verifyVPDChecksum(data []byte, readOnlyStart int, fields []vpdField) error {
	for i, field := range fields {
		if field.Keyword != vpdKeywordChecksum {
			continue
		}
		if i != len(fields)-1 {
			return fmt.Errorf("VPD %s field does not close VPD-R", vpdKeywordChecksum)
		}
		if len(field.Value) == 0 {
			return fmt.Errorf("VPD %s field holds no checksum", vpdKeywordChecksum)
		}
		var sum byte
		for _, b := range data[:readOnlyStart+field.offset+1] {
			sum += b
		}
		if sum != 0 {
			return fmt.Errorf("VPD checksum fails: the bytes through %s sum to 0x%02x", vpdKeywordChecksum, sum)
		}
		return nil
	}
	return fmt.Errorf("VPD-R has no %s field", vpdKeywordChecksum)
}

// parseVPDFields splits the body of a VPD-R or VPD-W resource into its
// fields: a two-character keyword, a one-byte length, the value.
func parseVPDFields(body []byte) ([]vpdField, error) {
	var fields []vpdField
	for off := 0; off < len(body); {
		if off+vpdFieldHeaderLen > len(body) {
			return nil, fmt.Errorf("truncated VPD field header at offset %d of its resource", off)
		}
		keyword := body[off : off+2]
		if !isVPDKeywordChar(keyword[0]) || !isVPDKeywordChar(keyword[1]) {
			return nil, fmt.Errorf("invalid VPD keyword 0x%02x%02x", keyword[0], keyword[1])
		}
		start := off + vpdFieldHeaderLen
		end := start + int(body[off+2])
		if end > len(body) {
			return nil, fmt.Errorf("VPD field %s exceeds its resource", keyword)
		}
		fields = append(fields, vpdField{Keyword: string(keyword), Value: body[start:end], offset: start})
		off = end
	}
	return fields, nil
}

func isVPDKeywordChar(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')
}

// mellanoxPortGUIDFromVPD derives the InfiniBand port GUID of a single-port
// Mellanox/NVIDIA adapter from its VPD image. V3 holds the adapter UUID as 32
// hex digits, whose last 12 are the adapter's base MAC; the GUID of port 1 is
// that MAC with 03:00 between its third and fourth byte. A VA field has to
// name V3 as the UUID. Anything else is an error: no GUID is better than a
// wrong one.
func mellanoxPortGUIDFromVPD(data []byte) (VPDPortGUID, error) {
	fields, err := parseVPDReadOnlyFields(data)
	if err != nil {
		return VPDPortGUID{}, err
	}

	values := map[string][]byte{}
	for _, field := range fields {
		switch field.Keyword {
		case vpdKeywordMellanoxUUID, vpdKeywordMellanoxAlias, vpdKeywordSerialNumber:
			if _, ok := values[field.Keyword]; ok {
				return VPDPortGUID{}, fmt.Errorf("VPD has more than one %s field", field.Keyword)
			}
			values[field.Keyword] = field.Value
		}
	}

	uuid, ok := values[vpdKeywordMellanoxUUID]
	if !ok {
		return VPDPortGUID{}, fmt.Errorf("VPD has no %s field", vpdKeywordMellanoxUUID)
	}
	if alias, ok := values[vpdKeywordMellanoxAlias]; ok && !vpdAliasNamesUUID(string(alias)) {
		return VPDPortGUID{}, fmt.Errorf("VPD %s field does not name %s", vpdKeywordMellanoxAlias, vpdMellanoxUUIDAlias)
	}
	if len(uuid) != uuidHexLen || !isHex(string(uuid)) {
		return VPDPortGUID{}, fmt.Errorf("VPD %s field is not a UUID of %d hex digits", vpdKeywordMellanoxUUID, uuidHexLen)
	}

	mac := strings.ToLower(string(uuid[uuidHexLen-macHexLen:]))
	if mac == strings.Repeat("0", macHexLen) || mac == strings.Repeat("f", macHexLen) {
		return VPDPortGUID{}, fmt.Errorf("VPD %s field holds no base MAC", vpdKeywordMellanoxUUID)
	}
	guid, err := normalizeGUID(mac[:6] + "0300" + mac[6:])
	if err != nil {
		return VPDPortGUID{}, err
	}
	return VPDPortGUID{
		GUID:         guid,
		SerialNumber: strings.TrimRight(string(values[vpdKeywordSerialNumber]), " \x00"),
		UUID:         strings.ToLower(string(uuid)),
	}, nil
}

// vpdAliasNamesUUID reports whether a VA value such as
// "MLX:MN=MLNX:CSKU=V2:UUID=V3:PCI=V0" names V3 as the UUID.
func vpdAliasNamesUUID(alias string) bool {
	for _, item := range strings.Split(alias, ":") {
		if item == vpdMellanoxUUIDAlias {
			return true
		}
	}
	return false
}

// normalizeGUID renders a GUID as 16 lowercase hex digits. It takes the
// digits with or without ":" or "-" separators and an optional 0x prefix,
// and refuses anything that is not 8 bytes or is all zeros.
func normalizeGUID(guid string) (string, error) {
	digits := strings.ToLower(strings.TrimSpace(guid))
	digits = strings.TrimPrefix(digits, "0x")
	digits = strings.NewReplacer(":", "", "-", "").Replace(digits)
	if len(digits) != guidHexLen || !isHex(digits) {
		return "", fmt.Errorf("%q is not a GUID of %d hex digits", guid, guidHexLen)
	}
	if digits == strings.Repeat("0", guidHexLen) {
		return "", errors.New("GUID is all zeros")
	}
	return digits, nil
}

// guidFromHardwareAddress returns the port GUID held in the last 8 bytes of
// a colon-separated hex address of wantBytes bytes: a 16-byte GID or a
// 20-byte IPoIB hardware address.
func guidFromHardwareAddress(address string, wantBytes int) (string, error) {
	digits := strings.ReplaceAll(strings.TrimSpace(address), ":", "")
	if len(digits) != 2*wantBytes || !isHex(digits) {
		return "", fmt.Errorf("%q is not an address of %d bytes", strings.TrimSpace(address), wantBytes)
	}
	return normalizeGUID(digits[len(digits)-guidHexLen:])
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
