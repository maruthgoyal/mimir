// Code generated by protoc-gen-gogo. DO NOT EDIT.
// source: cache.proto

package storepb

import (
	bytes "bytes"
	fmt "fmt"
	_ "github.com/gogo/protobuf/gogoproto"
	proto "github.com/gogo/protobuf/proto"
	_ "github.com/grafana/mimir/pkg/mimirpb"
	github_com_grafana_mimir_pkg_mimirpb "github.com/grafana/mimir/pkg/mimirpb"
	io "io"
	math "math"
	math_bits "math/bits"
	reflect "reflect"
	strings "strings"
)

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.GoGoProtoPackageIsVersion3 // please upgrade the proto package

type CachedSeries struct {
	// Keep reference to buffer for unsafe references.
	github_com_grafana_mimir_pkg_mimirpb.BufferHolder

	Series              []github_com_grafana_mimir_pkg_mimirpb.PreallocatingMetric `protobuf:"bytes,1,rep,name=series,proto3,customtype=github.com/grafana/mimir/pkg/mimirpb.PreallocatingMetric" json:"series"`
	DiffEncodedPostings []byte                                                     `protobuf:"bytes,5,opt,name=diffEncodedPostings,proto3" json:"diffEncodedPostings,omitempty"`
}

func (m *CachedSeries) Reset()      { *m = CachedSeries{} }
func (*CachedSeries) ProtoMessage() {}
func (*CachedSeries) Descriptor() ([]byte, []int) {
	return fileDescriptor_5fca3b110c9bbf3a, []int{0}
}
func (m *CachedSeries) XXX_Unmarshal(b []byte) error {
	return m.Unmarshal(b)
}
func (m *CachedSeries) XXX_Marshal(b []byte, deterministic bool) ([]byte, error) {
	if deterministic {
		return xxx_messageInfo_CachedSeries.Marshal(b, m, deterministic)
	} else {
		b = b[:cap(b)]
		n, err := m.MarshalToSizedBuffer(b)
		if err != nil {
			return nil, err
		}
		return b[:n], nil
	}
}
func (m *CachedSeries) XXX_Merge(src proto.Message) {
	xxx_messageInfo_CachedSeries.Merge(m, src)
}
func (m *CachedSeries) XXX_Size() int {
	return m.Size()
}
func (m *CachedSeries) XXX_DiscardUnknown() {
	xxx_messageInfo_CachedSeries.DiscardUnknown(m)
}

var xxx_messageInfo_CachedSeries proto.InternalMessageInfo

func init() {
	proto.RegisterType((*CachedSeries)(nil), "thanos.CachedSeries")
}

func init() { proto.RegisterFile("cache.proto", fileDescriptor_5fca3b110c9bbf3a) }

var fileDescriptor_5fca3b110c9bbf3a = []byte{
	// 295 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0x8c, 0x90, 0xbd, 0x4e, 0xc3, 0x30,
	0x10, 0x80, 0x6d, 0x9a, 0x96, 0x2a, 0xed, 0x10, 0x15, 0x86, 0xaa, 0xc3, 0xb5, 0x62, 0xea, 0x94,
	0x54, 0xb0, 0x30, 0x42, 0x11, 0x4b, 0x25, 0xa4, 0xaa, 0x6c, 0x6c, 0x8e, 0xe3, 0xba, 0x86, 0x26,
	0x8e, 0x6c, 0x23, 0x31, 0xf2, 0x08, 0x3c, 0x06, 0x4f, 0xc1, 0x9c, 0x31, 0x63, 0xc5, 0x50, 0x11,
	0x67, 0x61, 0xec, 0x23, 0xa0, 0x24, 0x8c, 0x0c, 0x4c, 0xf7, 0xe9, 0x7e, 0xbe, 0x3b, 0x9d, 0xdb,
	0xa3, 0x84, 0x6e, 0x98, 0x9f, 0x2a, 0x69, 0xe4, 0xa0, 0x63, 0x36, 0x24, 0x91, 0x7a, 0x34, 0xe3,
	0xc2, 0x6c, 0x9e, 0x43, 0x9f, 0xca, 0x38, 0xe0, 0x8a, 0xac, 0x49, 0x42, 0x82, 0x58, 0xc4, 0x42,
	0x05, 0xe9, 0x13, 0x6f, 0x28, 0x0d, 0x9b, 0xd8, 0x4c, 0x8e, 0x4e, 0xb9, 0xe4, 0xb2, 0xc6, 0xa0,
	0xa2, 0x26, 0x7b, 0xf6, 0x81, 0xdd, 0xfe, 0x4d, 0xe5, 0x8f, 0xee, 0x99, 0x12, 0x4c, 0x0f, 0x1e,
	0xdd, 0x8e, 0xae, 0x69, 0x88, 0x27, 0xad, 0x69, 0xef, 0xdc, 0xf3, 0xa9, 0x54, 0x86, 0xbd, 0xa4,
	0xa1, 0x7f, 0xc7, 0x8c, 0x12, 0x74, 0x7e, 0x95, 0xed, 0xc7, 0xe8, 0x73, 0x3f, 0xbe, 0xfc, 0xcf,
	0x09, 0xfe, 0x52, 0x31, 0xb2, 0xdd, 0x4a, 0x4a, 0x8c, 0x48, 0x78, 0x63, 0x58, 0xfd, 0x6e, 0x18,
	0xcc, 0xdc, 0x93, 0x48, 0xac, 0xd7, 0xb7, 0x09, 0x95, 0x11, 0x8b, 0x96, 0x52, 0x57, 0x3d, 0x7a,
	0xd8, 0x9e, 0xe0, 0x69, 0x7f, 0xf5, 0x57, 0x69, 0xe1, 0x74, 0x8f, 0xbc, 0xd6, 0xc2, 0xe9, 0xb6,
	0x3c, 0x67, 0xe1, 0x74, 0x1d, 0xaf, 0x3d, 0xbf, 0xce, 0x0a, 0x40, 0x79, 0x01, 0x68, 0x57, 0x00,
	0x3a, 0x14, 0x80, 0x5f, 0x2d, 0xe0, 0x77, 0x0b, 0x38, 0xb3, 0x80, 0x73, 0x0b, 0xf8, 0xcb, 0x02,
	0xfe, 0xb6, 0x80, 0x0e, 0x16, 0xf0, 0x5b, 0x09, 0x28, 0x2f, 0x01, 0xed, 0x4a, 0x40, 0x0f, 0xc7,
	0xda, 0x48, 0xc5, 0xd2, 0x30, 0xec, 0xd4, 0xaf, 0xb8, 0xf8, 0x09, 0x00, 0x00, 0xff, 0xff, 0x9f,
	0xa0, 0x0d, 0x09, 0x69, 0x01, 0x00, 0x00,
}

func (this *CachedSeries) Equal(that interface{}) bool {
	if that == nil {
		return this == nil
	}

	that1, ok := that.(*CachedSeries)
	if !ok {
		that2, ok := that.(CachedSeries)
		if ok {
			that1 = &that2
		} else {
			return false
		}
	}
	if that1 == nil {
		return this == nil
	} else if this == nil {
		return false
	}
	if len(this.Series) != len(that1.Series) {
		return false
	}
	for i := range this.Series {
		if !this.Series[i].Equal(that1.Series[i]) {
			return false
		}
	}
	if !bytes.Equal(this.DiffEncodedPostings, that1.DiffEncodedPostings) {
		return false
	}
	return true
}
func (this *CachedSeries) GoString() string {
	if this == nil {
		return "nil"
	}
	s := make([]string, 0, 6)
	s = append(s, "&storepb.CachedSeries{")
	s = append(s, "Series: "+fmt.Sprintf("%#v", this.Series)+",\n")
	s = append(s, "DiffEncodedPostings: "+fmt.Sprintf("%#v", this.DiffEncodedPostings)+",\n")
	s = append(s, "}")
	return strings.Join(s, "")
}
func valueToGoStringCache(v interface{}, typ string) string {
	rv := reflect.ValueOf(v)
	if rv.IsNil() {
		return "nil"
	}
	pv := reflect.Indirect(rv).Interface()
	return fmt.Sprintf("func(v %v) *%v { return &v } ( %#v )", typ, typ, pv)
}
func (m *CachedSeries) Marshal() (dAtA []byte, err error) {
	size := m.Size()
	dAtA = make([]byte, size)
	n, err := m.MarshalToSizedBuffer(dAtA[:size])
	if err != nil {
		return nil, err
	}
	return dAtA[:n], nil
}

func (m *CachedSeries) MarshalTo(dAtA []byte) (int, error) {
	size := m.Size()
	return m.MarshalToSizedBuffer(dAtA[:size])
}

func (m *CachedSeries) MarshalToSizedBuffer(dAtA []byte) (int, error) {
	i := len(dAtA)
	_ = i
	var l int
	_ = l
	if len(m.DiffEncodedPostings) > 0 {
		i -= len(m.DiffEncodedPostings)
		copy(dAtA[i:], m.DiffEncodedPostings)
		i = encodeVarintCache(dAtA, i, uint64(len(m.DiffEncodedPostings)))
		i--
		dAtA[i] = 0x2a
	}
	if len(m.Series) > 0 {
		for iNdEx := len(m.Series) - 1; iNdEx >= 0; iNdEx-- {
			{
				size := m.Series[iNdEx].Size()
				i -= size
				if _, err := m.Series[iNdEx].MarshalTo(dAtA[i:]); err != nil {
					return 0, err
				}
				i = encodeVarintCache(dAtA, i, uint64(size))
			}
			i--
			dAtA[i] = 0xa
		}
	}
	return len(dAtA) - i, nil
}

func encodeVarintCache(dAtA []byte, offset int, v uint64) int {
	offset -= sovCache(v)
	base := offset
	for v >= 1<<7 {
		dAtA[offset] = uint8(v&0x7f | 0x80)
		v >>= 7
		offset++
	}
	dAtA[offset] = uint8(v)
	return base
}
func (m *CachedSeries) Size() (n int) {
	if m == nil {
		return 0
	}
	var l int
	_ = l
	if len(m.Series) > 0 {
		for _, e := range m.Series {
			l = e.Size()
			n += 1 + l + sovCache(uint64(l))
		}
	}
	l = len(m.DiffEncodedPostings)
	if l > 0 {
		n += 1 + l + sovCache(uint64(l))
	}
	return n
}

func sovCache(x uint64) (n int) {
	return (math_bits.Len64(x|1) + 6) / 7
}
func sozCache(x uint64) (n int) {
	return sovCache(uint64((x << 1) ^ uint64((int64(x) >> 63))))
}
func (this *CachedSeries) String() string {
	if this == nil {
		return "nil"
	}
	s := strings.Join([]string{`&CachedSeries{`,
		`Series:` + fmt.Sprintf("%v", this.Series) + `,`,
		`DiffEncodedPostings:` + fmt.Sprintf("%v", this.DiffEncodedPostings) + `,`,
		`}`,
	}, "")
	return s
}
func valueToStringCache(v interface{}) string {
	rv := reflect.ValueOf(v)
	if rv.IsNil() {
		return "nil"
	}
	pv := reflect.Indirect(rv).Interface()
	return fmt.Sprintf("*%v", pv)
}
func (m *CachedSeries) Unmarshal(dAtA []byte) error {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		preIndex := iNdEx
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return ErrIntOverflowCache
			}
			if iNdEx >= l {
				return io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= uint64(b&0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		fieldNum := int32(wire >> 3)
		wireType := int(wire & 0x7)
		if wireType == 4 {
			return fmt.Errorf("proto: CachedSeries: wiretype end group for non-group")
		}
		if fieldNum <= 0 {
			return fmt.Errorf("proto: CachedSeries: illegal tag %d (wire type %d)", fieldNum, wire)
		}
		switch fieldNum {
		case 1:
			if wireType != 2 {
				return fmt.Errorf("proto: wrong wireType = %d for field Series", wireType)
			}
			var msglen int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowCache
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				msglen |= int(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if msglen < 0 {
				return ErrInvalidLengthCache
			}
			postIndex := iNdEx + msglen
			if postIndex < 0 {
				return ErrInvalidLengthCache
			}
			if postIndex > l {
				return io.ErrUnexpectedEOF
			}
			m.Series = append(m.Series, github_com_grafana_mimir_pkg_mimirpb.PreallocatingMetric{})
			if err := m.Series[len(m.Series)-1].Unmarshal(dAtA[iNdEx:postIndex]); err != nil {
				return err
			}
			iNdEx = postIndex
		case 5:
			if wireType != 2 {
				return fmt.Errorf("proto: wrong wireType = %d for field DiffEncodedPostings", wireType)
			}
			var byteLen int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowCache
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				byteLen |= int(b&0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if byteLen < 0 {
				return ErrInvalidLengthCache
			}
			postIndex := iNdEx + byteLen
			if postIndex < 0 {
				return ErrInvalidLengthCache
			}
			if postIndex > l {
				return io.ErrUnexpectedEOF
			}
			m.DiffEncodedPostings = append(m.DiffEncodedPostings[:0], dAtA[iNdEx:postIndex]...)
			if m.DiffEncodedPostings == nil {
				m.DiffEncodedPostings = []byte{}
			}
			iNdEx = postIndex
		default:
			iNdEx = preIndex
			skippy, err := skipCache(dAtA[iNdEx:])
			if err != nil {
				return err
			}
			if (skippy < 0) || (iNdEx+skippy) < 0 {
				return ErrInvalidLengthCache
			}
			if (iNdEx + skippy) > l {
				return io.ErrUnexpectedEOF
			}
			iNdEx += skippy
		}
	}

	if iNdEx > l {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func skipCache(dAtA []byte) (n int, err error) {
	l := len(dAtA)
	iNdEx := 0
	depth := 0
	for iNdEx < l {
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return 0, ErrIntOverflowCache
			}
			if iNdEx >= l {
				return 0, io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= (uint64(b) & 0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		wireType := int(wire & 0x7)
		switch wireType {
		case 0:
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowCache
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				iNdEx++
				if dAtA[iNdEx-1] < 0x80 {
					break
				}
			}
		case 1:
			iNdEx += 8
		case 2:
			var length int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowCache
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				length |= (int(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if length < 0 {
				return 0, ErrInvalidLengthCache
			}
			iNdEx += length
		case 3:
			depth++
		case 4:
			if depth == 0 {
				return 0, ErrUnexpectedEndOfGroupCache
			}
			depth--
		case 5:
			iNdEx += 4
		default:
			return 0, fmt.Errorf("proto: illegal wireType %d", wireType)
		}
		if iNdEx < 0 {
			return 0, ErrInvalidLengthCache
		}
		if depth == 0 {
			return iNdEx, nil
		}
	}
	return 0, io.ErrUnexpectedEOF
}

var (
	ErrInvalidLengthCache        = fmt.Errorf("proto: negative length found during unmarshaling")
	ErrIntOverflowCache          = fmt.Errorf("proto: integer overflow")
	ErrUnexpectedEndOfGroupCache = fmt.Errorf("proto: unexpected end of group")
)
