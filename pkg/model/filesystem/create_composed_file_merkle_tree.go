package filesystem

import (
	"bytes"
	"context"
	"io"

	"bonanza.build/pkg/encoding/utf8stat"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/btree"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	// maximumSuffixSizeBytes is the maximum size of literal data that
	// is stored as the suffix of a slice. Larger runs of data, and
	// data that is not preceded by a slice, are stored as chunks.
	maximumSuffixSizeBytes = 1 << 10

	// maximumComposedChunkSizeBytes is the maximum size of chunks
	// that are created to store data of composed files that is too
	// large to be stored inline.
	maximumComposedChunkSizeBytes = 1 << 16
)

// ComposedFileMerkleTreeCapturer is used by
// CreateComposedFileMerkleTree() to capture the objects that it
// creates, and to obtain metadata for the objects of the source files
// that slices refer to.
type ComposedFileMerkleTreeCapturer[TReference, TMetadata any] interface {
	FileMerkleTreeCapturer[TMetadata]
	model_core.ExistingObjectCapturer[TReference, TMetadata]
}

type pendingComposedPartKind int

const (
	pendingComposedPartNone pendingComposedPartKind = iota
	pendingComposedPartHole
	pendingComposedPartData
	pendingComposedPartSlice
)

type composedFileMerkleTreeBuilder[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata] struct {
	context     context.Context
	parameters  *FileCreationParameters
	capturer    ComposedFileMerkleTreeCapturer[TReference, TMetadata]
	sources     []FileContentsEntry[TReference]
	treeBuilder btree.Builder[*model_filesystem_pb.FileContents, TMetadata]

	// The first part of the file is not pushed into the B-tree
	// immediately, as files consisting of a single part need to be
	// represented differently.
	firstPart    model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]
	hasFirstPart bool
	partsCount   int

	// The part that is currently being accumulated. Adjacent holes
	// and adjacent runs of data are coalesced, as are slices that
	// are contiguous within the same source.
	pendingKind        pendingComposedPartKind
	pendingSizeBytes   uint64
	pendingData        []byte
	pendingInput       uint64
	pendingOffsetBytes uint64
	pendingSuffix      []byte
}

func (b *composedFileMerkleTreeBuilder[TReference, TMetadata]) pushPart(part model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]) error {
	b.partsCount++
	if b.partsCount == 1 {
		b.firstPart = part
		b.hasFirstPart = true
		return nil
	}
	if b.hasFirstPart {
		b.hasFirstPart = false
		if err := b.treeBuilder.PushChild(b.firstPart); err != nil {
			part.Discard()
			return err
		}
	}
	return b.treeBuilder.PushChild(part)
}

// newChunkPart stores data in a separate chunk object, and returns a
// part referring to it.
func (b *composedFileMerkleTreeBuilder[TReference, TMetadata]) newChunkPart(data []byte) (model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata], error) {
	encodedChunk, err := b.parameters.EncodeChunk(data)
	if err != nil {
		return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
	}
	chunkMetadata, err := b.capturer.CaptureChunk(b.context, encodedChunk.Value)
	if err != nil {
		return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
	}
	return model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_filesystem_pb.FileContents {
		return &model_filesystem_pb.FileContents{
			Level: &model_filesystem_pb.FileContents_ChunkReference{
				ChunkReference: &model_core_pb.DecodableReference{
					Reference: patcher.AddReference(model_core.MetadataEntry[TMetadata]{
						LocalReference: encodedChunk.Value.GetLocalReference(),
						Metadata:       chunkMetadata,
					}),
					DecodingParameters: encodedChunk.GetDecodingParameters(),
				},
			},
			TotalSizeBytes: uint64(len(data)),
			Utf8Stat:       uint64(utf8stat.Compute(data)),
		}
	}), nil
}

// newSlicePart creates a part that refers to a range of bytes of a
// source file, followed by a suffix. The message of the source file's
// contents is constructed from scratch, as opposed to cloning the
// original message, as composed files may contain millions of slices.
func (b *composedFileMerkleTreeBuilder[TReference, TMetadata]) newSlicePart(input, offsetBytes, sizeBytes uint64, suffix []byte) model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata] {
	source := &b.sources[input]
	reference := source.reference
	return model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_filesystem_pb.FileContents {
		sourceReference := &model_core_pb.DecodableReference{
			Reference: patcher.AddReference(model_core.MetadataEntry[TMetadata]{
				LocalReference: reference.Value.GetLocalReference(),
				Metadata:       b.capturer.CaptureExistingObject(reference.Value),
			}),
			DecodingParameters: reference.GetDecodingParameters(),
		}
		sourceContents := &model_filesystem_pb.FileContents{
			TotalSizeBytes: source.endBytes,
			Utf8Stat:       uint64(source.utf8Stat),
		}
		if reference.Value.GetHeight() > 0 {
			sourceContents.Level = &model_filesystem_pb.FileContents_List_{
				List: &model_filesystem_pb.FileContents_List{
					Reference: sourceReference,
					Sparse:    source.contiguousCount == 0,
				},
			}
		} else {
			sourceContents.Level = &model_filesystem_pb.FileContents_ChunkReference{
				ChunkReference: sourceReference,
			}
		}
		return &model_filesystem_pb.FileContents{
			Level: &model_filesystem_pb.FileContents_Slice_{
				Slice: &model_filesystem_pb.FileContents_Slice{
					Source:      sourceContents,
					OffsetBytes: offsetBytes,
					Suffix:      suffix,
				},
			},
			TotalSizeBytes: sizeBytes + uint64(len(suffix)),
		}
	})
}

// flushPending emits the part that is currently being accumulated.
func (b *composedFileMerkleTreeBuilder[TReference, TMetadata]) flushPending() error {
	kind := b.pendingKind
	b.pendingKind = pendingComposedPartNone
	switch kind {
	case pendingComposedPartHole:
		return b.pushPart(model_core.NewSimplePatchedMessage[TMetadata](&model_filesystem_pb.FileContents{
			Level: &model_filesystem_pb.FileContents_Hole{
				Hole: &emptypb.Empty{},
			},
			TotalSizeBytes: b.pendingSizeBytes,
		}))
	case pendingComposedPartData:
		data := b.pendingData
		b.pendingData = nil
		for len(data) > 0 {
			piece := data[:min(len(data), maximumComposedChunkSizeBytes)]
			data = data[len(piece):]
			part, err := b.newChunkPart(piece)
			if err != nil {
				return err
			}
			if err := b.pushPart(part); err != nil {
				return err
			}
		}
		return nil
	case pendingComposedPartSlice:
		suffix := b.pendingSuffix
		b.pendingSuffix = nil
		return b.pushPart(b.newSlicePart(b.pendingInput, b.pendingOffsetBytes, b.pendingSizeBytes, suffix))
	default:
		return nil
	}
}

func (b *composedFileMerkleTreeBuilder[TReference, TMetadata]) addHole(sizeBytes uint64) error {
	if b.pendingKind != pendingComposedPartHole {
		if err := b.flushPending(); err != nil {
			return err
		}
		b.pendingKind = pendingComposedPartHole
		b.pendingSizeBytes = 0
	}
	b.pendingSizeBytes += sizeBytes
	return nil
}

func (b *composedFileMerkleTreeBuilder[TReference, TMetadata]) addData(data []byte) error {
	switch b.pendingKind {
	case pendingComposedPartSlice:
		if len(b.pendingSuffix)+len(data) <= maximumSuffixSizeBytes {
			// Small amounts of data following a slice are
			// stored as the suffix of the slice.
			b.pendingSuffix = append(b.pendingSuffix, data...)
			return nil
		}
		if err := b.flushPending(); err != nil {
			return err
		}
		b.pendingKind = pendingComposedPartData
	case pendingComposedPartData:
	default:
		if err := b.flushPending(); err != nil {
			return err
		}
		b.pendingKind = pendingComposedPartData
	}
	b.pendingData = append(b.pendingData, data...)
	if len(b.pendingData) >= maximumComposedChunkSizeBytes {
		return b.flushPending()
	}
	return nil
}

func (b *composedFileMerkleTreeBuilder[TReference, TMetadata]) addSlice(input, offsetBytes, sizeBytes uint64) error {
	if b.pendingKind == pendingComposedPartSlice && len(b.pendingSuffix) == 0 && b.pendingInput == input && b.pendingOffsetBytes+b.pendingSizeBytes == offsetBytes {
		// Extend the previous slice, as it is contiguous with
		// the current one.
		b.pendingSizeBytes += sizeBytes
		return nil
	}
	if err := b.flushPending(); err != nil {
		return err
	}
	b.pendingKind = pendingComposedPartSlice
	b.pendingInput = input
	b.pendingOffsetBytes = offsetBytes
	b.pendingSizeBytes = sizeBytes
	return nil
}

// CreateComposedFileMerkleTree creates a Merkle tree for a file whose
// contents are described by a segment list, without reading the
// contents of the source files. Ranges of bytes of source files are
// stored as slices, small amounts of literal data following them are
// stored as suffixes of these slices, other literal data is stored as
// chunks, and runs of null bytes are stored as holes. Reading the resulting
// file causes the data of the source files to be read on demand.
//
// Sources may not themselves contain slices. This is not validated by
// this function, but by FileReader when reading the resulting file.
func CreateComposedFileMerkleTree[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata](
	ctx context.Context,
	parameters *FileCreationParameters,
	segments *SegmentListReader,
	sources []model_core.Message[*model_filesystem_pb.FileContents, TReference],
	capturer ComposedFileMerkleTreeCapturer[TReference, TMetadata],
) (model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata], error) {
	if inputsCount := segments.GetInputsCount(); uint64(len(sources)) != inputsCount {
		return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, status.Errorf(codes.InvalidArgument, "Segment list expects %d inputs, while %d sources were provided", inputsCount, len(sources))
	}
	sourceEntries := make([]FileContentsEntry[TReference], 0, len(sources))
	for i, source := range sources {
		entry, err := NewFileContentsEntryFromProto(source)
		if err != nil {
			return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, util.StatusWrapf(err, "Invalid contents of source %d", i)
		}
		sourceEntries = append(sourceEntries, entry)
	}

	b := composedFileMerkleTreeBuilder[TReference, TMetadata]{
		context:     ctx,
		parameters:  parameters,
		capturer:    capturer,
		sources:     sourceEntries,
		treeBuilder: newFileContentsListBuilder(ctx, parameters, capturer),
	}
	defer func() {
		b.treeBuilder.Discard()
		if b.hasFirstPart {
			b.firstPart.Discard()
		}
	}()

	for {
		if err := util.StatusFromContext(ctx); err != nil {
			return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
		}
		segment, err := segments.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
		}

		switch segment.Kind {
		case SegmentKindSlice:
			source := &sourceEntries[segment.Input]
			sourceSizeBytes := source.GetEndBytes()
			if segment.OffsetBytes > sourceSizeBytes || segment.SizeBytes > sourceSizeBytes-segment.OffsetBytes {
				return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, status.Errorf(codes.InvalidArgument, "Slice at offset %d with size %d exceeds the size of input %d, which is %d bytes", segment.OffsetBytes, segment.SizeBytes, segment.Input, sourceSizeBytes)
			}
			if source.IsHole() {
				// Slices of files consisting of null
				// bytes are holes.
				err = b.addHole(segment.SizeBytes)
			} else {
				err = b.addSlice(segment.Input, segment.OffsetBytes, segment.SizeBytes)
			}
		case SegmentKindInline:
			err = b.addData(segment.Data)
		case SegmentKindFill:
			if segment.FillByte == 0 {
				err = b.addHole(segment.SizeBytes)
			} else {
				// Expand runs of non-null bytes in
				// pieces, so that their size does not
				// affect memory usage.
				piece := bytes.Repeat([]byte{segment.FillByte}, int(min(segment.SizeBytes, maximumComposedChunkSizeBytes)))
				for remainingBytes := segment.SizeBytes; remainingBytes > 0 && err == nil; {
					n := min(remainingBytes, uint64(len(piece)))
					err = b.addData(piece[:n])
					remainingBytes -= n
				}
			}
		}
		if err != nil {
			return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
		}
	}
	if err := b.flushPending(); err != nil {
		return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
	}

	switch b.partsCount {
	case 0:
		// Empty file.
		return model_core.NewSimplePatchedMessage[TMetadata]((*model_filesystem_pb.FileContents)(nil)), nil
	case 1:
		// File consisting of a single part. Slices can only be
		// stored in file contents lists, so they need to be
		// converted.
		part := b.firstPart
		b.hasFirstPart = false
		slice, ok := part.Message.Level.(*model_filesystem_pb.FileContents_Slice_)
		if !ok {
			// Holes and chunks can be stored as is.
			return part, nil
		}
		suffix := slice.Slice.Suffix
		offsetBytes := slice.Slice.OffsetBytes
		sizeBytes := part.Message.TotalSizeBytes - uint64(len(suffix))
		part.Discard()
		source := &b.sources[b.pendingInput]
		if len(suffix) == 0 && offsetBytes == 0 && sizeBytes == source.endBytes {
			// Slice covers the entire source file. Use the
			// contents of the source file.
			return model_core.Patch(capturer, sources[b.pendingInput]), nil
		}
		// Split the part in two, so that it can be stored in a
		// file contents list.
		if len(suffix) > 0 {
			if err := b.treeBuilder.PushChild(b.newSlicePart(b.pendingInput, offsetBytes, sizeBytes, nil)); err != nil {
				return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
			}
			chunk, err := b.newChunkPart(suffix)
			if err != nil {
				return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
			}
			if err := b.treeBuilder.PushChild(chunk); err != nil {
				return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
			}
		} else {
			if sizeBytes < 2 {
				return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, status.Error(codes.Unimplemented, "Composed files consisting of a single byte of a larger source file are not supported")
			}
			for _, half := range [][2]uint64{
				{offsetBytes, sizeBytes / 2},
				{offsetBytes + sizeBytes/2, sizeBytes - sizeBytes/2},
			} {
				if err := b.treeBuilder.PushChild(b.newSlicePart(b.pendingInput, half[0], half[1], nil)); err != nil {
					return model_core.PatchedMessage[*model_filesystem_pb.FileContents, TMetadata]{}, err
				}
			}
		}
		return b.treeBuilder.FinalizeSingle()
	default:
		return b.treeBuilder.FinalizeSingle()
	}
}
