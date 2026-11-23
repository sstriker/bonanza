package filesystem_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	model_filesystem "bonanza.build/pkg/model/filesystem"

	"github.com/stretchr/testify/require"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// segmentListBuilder is a helper for constructing segment lists in
// unit tests.
type segmentListBuilder struct {
	segments       []byte
	segmentsCount  uint64
	totalSizeBytes uint64
}

func (b *segmentListBuilder) slice(input, offsetBytes, sizeBytes uint64) *segmentListBuilder {
	b.segments = append(b.segments, 0)
	b.segments = binary.AppendUvarint(b.segments, input)
	b.segments = binary.AppendUvarint(b.segments, offsetBytes)
	b.segments = binary.AppendUvarint(b.segments, sizeBytes)
	b.segmentsCount++
	b.totalSizeBytes += sizeBytes
	return b
}

func (b *segmentListBuilder) inline(data []byte) *segmentListBuilder {
	b.segments = append(b.segments, 1)
	b.segments = binary.AppendUvarint(b.segments, uint64(len(data)))
	b.segments = append(b.segments, data...)
	b.segmentsCount++
	b.totalSizeBytes += uint64(len(data))
	return b
}

func (b *segmentListBuilder) fill(value byte, sizeBytes uint64) *segmentListBuilder {
	b.segments = append(b.segments, 2, value)
	b.segments = binary.AppendUvarint(b.segments, sizeBytes)
	b.segmentsCount++
	b.totalSizeBytes += sizeBytes
	return b
}

func (b *segmentListBuilder) build(inputsCount uint64) []byte {
	return b.buildWithTotalSize(inputsCount, b.totalSizeBytes)
}

func (b *segmentListBuilder) buildWithTotalSize(inputsCount, totalSizeBytes uint64) []byte {
	out := []byte("RLSG\x01\x00")
	out = binary.AppendUvarint(out, totalSizeBytes)
	out = binary.AppendUvarint(out, inputsCount)
	out = binary.AppendUvarint(out, b.segmentsCount)
	return append(out, b.segments...)
}

func readAllSegments(data []byte) ([]model_filesystem.Segment, error) {
	r, err := model_filesystem.NewSegmentListReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var segments []model_filesystem.Segment
	for {
		segment, err := r.Next()
		if err == io.EOF {
			return segments, nil
		} else if err != nil {
			return segments, err
		}
		segments = append(segments, segment)
	}
}

func TestSegmentListReader(t *testing.T) {
	t.Run("Valid", func(t *testing.T) {
		data := (&segmentListBuilder{}).
			slice(1, 100, 20).
			inline([]byte("Hello")).
			fill(0, 1000).
			fill(0x90, 3).
			build(2)
		r, err := model_filesystem.NewSegmentListReader(bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, uint64(1028), r.GetTotalSizeBytes())
		require.Equal(t, uint64(2), r.GetInputsCount())

		segments, err := readAllSegments(data)
		require.NoError(t, err)
		require.Equal(t, []model_filesystem.Segment{
			{Kind: model_filesystem.SegmentKindSlice, SizeBytes: 20, Input: 1, OffsetBytes: 100},
			{Kind: model_filesystem.SegmentKindInline, SizeBytes: 5, Data: []byte("Hello")},
			{Kind: model_filesystem.SegmentKindFill, SizeBytes: 1000},
			{Kind: model_filesystem.SegmentKindFill, SizeBytes: 3, FillByte: 0x90},
		}, segments)
	})

	t.Run("Empty", func(t *testing.T) {
		segments, err := readAllSegments((&segmentListBuilder{}).build(0))
		require.NoError(t, err)
		require.Empty(t, segments)
	})

	for name, tc := range map[string]struct {
		data []byte
		code codes.Code
	}{
		"BadMagic":         {[]byte("RLSX\x01\x00\x00\x00\x00"), codes.InvalidArgument},
		"BadVersion":       {[]byte("RLSG\x02\x00\x00\x00\x00"), codes.InvalidArgument},
		"TruncatedHeader":  {[]byte("RLSG\x01"), codes.InvalidArgument},
		"InputOutOfRange":  {(&segmentListBuilder{}).slice(2, 0, 1).build(2), codes.InvalidArgument},
		"ZeroLengthSlice":  {(&segmentListBuilder{}).slice(0, 0, 0).build(1), codes.InvalidArgument},
		"ZeroLengthFill":   {(&segmentListBuilder{}).fill(1, 0).build(0), codes.InvalidArgument},
		"ZeroLengthInline": {(&segmentListBuilder{}).inline(nil).build(0), codes.InvalidArgument},
		"UnknownKind":      {append((&segmentListBuilder{segmentsCount: 1}).buildWithTotalSize(0, 1), 3, 1), codes.InvalidArgument},
		"SizeTooSmall":     {(&segmentListBuilder{}).fill(1, 10).buildWithTotalSize(0, 11), codes.InvalidArgument},
		"SizeTooLarge":     {(&segmentListBuilder{}).fill(1, 10).buildWithTotalSize(0, 9), codes.InvalidArgument},
		"TrailingData":     {append((&segmentListBuilder{}).fill(1, 10).build(0), 0), codes.InvalidArgument},
		"TruncatedInline":  {(&segmentListBuilder{}).inline([]byte("Hello")).build(0)[:15], codes.InvalidArgument},
		// An inline segment whose size exceeds the remaining
		// size should be rejected before allocating memory.
		"HugeInline": {append((&segmentListBuilder{segmentsCount: 1}).buildWithTotalSize(0, 10), 1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f), codes.InvalidArgument},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readAllSegments(tc.data)
			require.Error(t, err)
			require.Equal(t, tc.code, status.Code(err), err.Error())
		})
	}
}
