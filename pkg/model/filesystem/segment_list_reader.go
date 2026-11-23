package filesystem

import (
	"encoding/binary"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SegmentKind indicates how the data of a segment in a segment list is
// obtained.
type SegmentKind int

const (
	// SegmentKindSlice indicates that the data of the segment is a
	// range of bytes of one of the input files.
	SegmentKindSlice SegmentKind = iota
	// SegmentKindInline indicates that the data of the segment is
	// stored in the segment list itself.
	SegmentKindInline
	// SegmentKindFill indicates that the segment consists of a
	// single byte value that is repeated.
	SegmentKindFill
)

// Segment of a composed file, as returned by SegmentListReader.
type Segment struct {
	Kind      SegmentKind
	SizeBytes uint64

	// For SegmentKindSlice, the index of the input file and the
	// offset within the input file at which the slice starts.
	Input       uint64
	OffsetBytes uint64

	// For SegmentKindInline, the literal data of the segment.
	Data []byte

	// For SegmentKindFill, the byte value that is repeated.
	FillByte byte
}

// SegmentListReader reads segment lists, which describe how a file
// can be composed out of ranges of bytes of other files, data that is
// stored inline, and runs of repeated bytes. These lists are generated
// by tools like relink, so that linked executables don't need to be
// written out and stored, if their contents are already present in
// the object files from which they are linked.
//
// The format is as follows, with all integers encoded as unsigned
// LEB128 varints unless noted otherwise:
//
//	magic "RLSG", version (uint16, little endian) = 1
//	total size in bytes
//	number of inputs
//	number of segments
//	segments, each starting with a kind byte:
//	  0 = slice:  input index, offset, size
//	  1 = inline: size, followed by the data
//	  2 = fill:   byte value (one byte), size
//
// Sizes of segments MUST be greater than zero, and MUST sum up to the
// total size. No data may follow the last segment.
type SegmentListReader struct {
	r                 io.ByteReader
	totalSizeBytes    uint64
	inputsCount       uint64
	remainingSegments uint64
	remainingBytes    uint64
}

const segmentListMagic = "RLSG"

// NewSegmentListReader creates a SegmentListReader that reads a segment
// list from the provided reader. The header of the segment list is
// read immediately.
func NewSegmentListReader(r io.ByteReader) (*SegmentListReader, error) {
	var header [6]byte
	for i := range header {
		b, err := r.ReadByte()
		if err != nil {
			return nil, segmentListReadError(err, "header")
		}
		header[i] = b
	}
	if string(header[:4]) != segmentListMagic {
		return nil, status.Error(codes.InvalidArgument, "Segment list does not start with magic \"RLSG\"")
	}
	if version := binary.LittleEndian.Uint16(header[4:]); version != 1 {
		return nil, status.Errorf(codes.InvalidArgument, "Unsupported segment list version %d", version)
	}
	totalSizeBytes, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, segmentListReadError(err, "total size")
	}
	inputsCount, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, segmentListReadError(err, "number of inputs")
	}
	segmentsCount, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, segmentListReadError(err, "number of segments")
	}
	return &SegmentListReader{
		r:                 r,
		totalSizeBytes:    totalSizeBytes,
		inputsCount:       inputsCount,
		remainingSegments: segmentsCount,
		remainingBytes:    totalSizeBytes,
	}, nil
}

func segmentListReadError(err error, field string) error {
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	return status.Errorf(codes.InvalidArgument, "Failed to read %s of segment list: %s", field, err)
}

// GetTotalSizeBytes returns the size of the composed file.
func (r *SegmentListReader) GetTotalSizeBytes() uint64 {
	return r.totalSizeBytes
}

// GetInputsCount returns the number of input files the segment list
// refers to.
func (r *SegmentListReader) GetInputsCount() uint64 {
	return r.inputsCount
}

// Next returns the next segment in the segment list. Once all segments
// have been returned, io.EOF is returned, after validating that the
// sizes of all segments add up to the total size, and that no trailing
// data is present.
func (r *SegmentListReader) Next() (Segment, error) {
	if r.remainingSegments == 0 {
		if r.remainingBytes != 0 {
			return Segment{}, status.Errorf(codes.InvalidArgument, "Segments have a total size of %d bytes, while %d bytes were expected", r.totalSizeBytes-r.remainingBytes, r.totalSizeBytes)
		}
		if _, err := r.r.ReadByte(); err != io.EOF {
			if err != nil {
				return Segment{}, segmentListReadError(err, "end")
			}
			return Segment{}, status.Error(codes.InvalidArgument, "Segment list contains trailing data")
		}
		return Segment{}, io.EOF
	}

	kind, err := r.r.ReadByte()
	if err != nil {
		return Segment{}, segmentListReadError(err, "segment kind")
	}
	var segment Segment
	switch kind {
	case 0:
		segment.Kind = SegmentKindSlice
		if segment.Input, err = binary.ReadUvarint(r.r); err != nil {
			return Segment{}, segmentListReadError(err, "slice input")
		}
		if segment.Input >= r.inputsCount {
			return Segment{}, status.Errorf(codes.InvalidArgument, "Slice refers to input %d, while the segment list only has %d inputs", segment.Input, r.inputsCount)
		}
		if segment.OffsetBytes, err = binary.ReadUvarint(r.r); err != nil {
			return Segment{}, segmentListReadError(err, "slice offset")
		}
		if segment.SizeBytes, err = binary.ReadUvarint(r.r); err != nil {
			return Segment{}, segmentListReadError(err, "slice size")
		}
	case 1:
		segment.Kind = SegmentKindInline
		if segment.SizeBytes, err = binary.ReadUvarint(r.r); err != nil {
			return Segment{}, segmentListReadError(err, "inline size")
		}
		if segment.SizeBytes > r.remainingBytes {
			// Check this before allocating memory.
			return Segment{}, status.Errorf(codes.InvalidArgument, "Segment of %d bytes exceeds the remaining size of %d bytes", segment.SizeBytes, r.remainingBytes)
		}
		segment.Data = make([]byte, segment.SizeBytes)
		for i := range segment.Data {
			if segment.Data[i], err = r.r.ReadByte(); err != nil {
				return Segment{}, segmentListReadError(err, "inline data")
			}
		}
	case 2:
		segment.Kind = SegmentKindFill
		if segment.FillByte, err = r.r.ReadByte(); err != nil {
			return Segment{}, segmentListReadError(err, "fill byte")
		}
		if segment.SizeBytes, err = binary.ReadUvarint(r.r); err != nil {
			return Segment{}, segmentListReadError(err, "fill size")
		}
	default:
		return Segment{}, status.Errorf(codes.InvalidArgument, "Unknown segment kind %d", kind)
	}
	if segment.SizeBytes == 0 {
		return Segment{}, status.Error(codes.InvalidArgument, "Segment has a size of zero bytes")
	}
	if segment.SizeBytes > r.remainingBytes {
		return Segment{}, status.Errorf(codes.InvalidArgument, "Segment of %d bytes exceeds the remaining size of %d bytes", segment.SizeBytes, r.remainingBytes)
	}
	r.remainingBytes -= segment.SizeBytes
	r.remainingSegments--
	return segment, nil
}
