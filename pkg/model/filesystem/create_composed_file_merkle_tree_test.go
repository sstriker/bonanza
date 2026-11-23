package filesystem_test

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"testing"

	model_core "bonanza.build/pkg/model/core"
	model_encoding "bonanza.build/pkg/model/encoding"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_chunking_pb "bonanza.build/pkg/proto/model/chunking"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	object_pb "bonanza.build/pkg/proto/storage/object"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"

	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// composedFileObjectStore is an in-memory object store that is used to
// test the creation and reading of composed files.
type composedFileObjectStore struct {
	objects map[object.LocalReference]*object.Contents
}

func (s *composedFileObjectStore) CaptureChunk(ctx context.Context, contents *object.Contents) (model_core.NoopReferenceMetadata, error) {
	s.objects[contents.GetLocalReference()] = contents
	return model_core.NoopReferenceMetadata{}, nil
}

func (s *composedFileObjectStore) CaptureFileContentsList(ctx context.Context, createdObject model_core.CreatedObject[model_core.NoopReferenceMetadata]) (model_core.NoopReferenceMetadata, error) {
	s.objects[createdObject.Contents.GetLocalReference()] = createdObject.Contents
	return model_core.NoopReferenceMetadata{}, nil
}

func (*composedFileObjectStore) CaptureExistingObject(object.LocalReference) model_core.NoopReferenceMetadata {
	return model_core.NoopReferenceMetadata{}
}

func (s *composedFileObjectStore) ReadObject(ctx context.Context, reference object.LocalReference) (model_core.Message[[]byte, object.LocalReference], error) {
	contents, ok := s.objects[reference]
	if !ok {
		return model_core.Message[[]byte, object.LocalReference]{}, status.Error(codes.NotFound, "Object not found")
	}
	return model_core.NewRawMessage(contents), nil
}

func TestCreateComposedFileMerkleTree(t *testing.T) {
	ctx := t.Context()
	referenceFormat := util.Must(object.NewReferenceFormat(object_pb.ReferenceFormat_SHA256_V1))
	binaryEncoderFactory := model_encoding.NewBinaryEncoderFactory(true)
	fileCreationParametersMessage := &model_filesystem_pb.FileCreationParameters{
		Access: &model_filesystem_pb.FileAccessParameters{},
		// Use small chunks and lists, so that both source files
		// and composed files span multiple objects.
		ChunkParameters: &model_chunking_pb.Parameters{
			MinimumSizeBytes: 1 << 10,
			HorizonSizeBytes: 1 << 13,
		},
		FileContentsListParameters: &model_chunking_pb.Parameters{
			MinimumSizeBytes: 1 << 10,
			HorizonSizeBytes: 1 << 13,
		},
	}
	fileCreationParameters := util.Must(model_filesystem.NewFileCreationParametersFromProto(fileCreationParametersMessage, referenceFormat, binaryEncoderFactory))
	fileAccessParameters := util.Must(model_filesystem.NewFileAccessParametersFromProto(fileCreationParametersMessage.Access, referenceFormat, binaryEncoderFactory))

	store := &composedFileObjectStore{objects: map[object.LocalReference]*object.Contents{}}
	fileReader := model_filesystem.NewFileReader(
		model_parser.NewParsedObjectReader(
			store,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[object.LocalReference](fileAccessParameters.GetFileContentsListEncoder()),
				model_filesystem.NewFileContentsListObjectParser[object.LocalReference](),
			),
		),
		model_parser.NewParsedObjectReader(
			store,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[object.LocalReference](fileAccessParameters.GetChunkEncoder()),
				model_parser.NewRawObjectParser[object.LocalReference](),
			),
		),
		semaphore.NewWeighted(4),
	)

	// createFile writes a file to the store, returning its contents
	// in the form that is stored in file properties.
	createFile := func(t *testing.T, data []byte) model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference] {
		fileContents, err := model_filesystem.CreateFileMerkleTree(ctx, fileCreationParameters, bytes.NewReader(data), store)
		require.NoError(t, err)
		topLevel, _ := fileContents.SortAndSetReferences()
		return topLevel.Decay()
	}
	compose := func(segments []byte, sources []model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference]) (model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference], error) {
		r, err := model_filesystem.NewSegmentListReader(bytes.NewReader(segments))
		if err != nil {
			return model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference]{}, err
		}
		fileContents, err := model_filesystem.CreateComposedFileMerkleTree(ctx, fileCreationParameters, r, sources, store)
		if err != nil {
			return model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference]{}, err
		}
		topLevel, _ := fileContents.SortAndSetReferences()
		return topLevel.Decay(), nil
	}
	// requireContents checks that a file can be read back using all
	// of the methods provided by FileReader.
	requireContents := func(t *testing.T, expected []byte, fileContents model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference]) {
		entry, err := model_filesystem.NewFileContentsEntryFromProto(fileContents)
		require.NoError(t, err)
		require.Equal(t, uint64(len(expected)), entry.GetEndBytes())

		actual, err := fileReader.FileReadAll(ctx, entry, 1<<30)
		require.NoError(t, err)
		require.Equal(t, expected, actual)

		sequential, err := io.ReadAll(fileReader.FileOpenRead(ctx, entry, 0))
		require.NoError(t, err)
		require.Equal(t, expected, sequential)

		rng := rand.New(rand.NewPCG(1, 2))
		for range 100 {
			offset := rng.IntN(len(expected))
			p := make([]byte, rng.IntN(len(expected)-offset)+1)
			n, err := fileReader.FileReadAt(ctx, entry, p, uint64(offset))
			require.NoError(t, err)
			require.Equal(t, expected[offset:][:n], p)
		}
	}

	// Source files. The first consists of random data, the second
	// is large enough to be split into multiple chunks, and the
	// third consists entirely of null bytes.
	rng := rand.New(rand.NewChaCha8([32]byte{}))
	source0Data := make([]byte, 100000)
	for i := range source0Data {
		source0Data[i] = byte(rng.Uint32())
	}
	source1Data := bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog. "), 2000)
	source2Data := make([]byte, 5000)
	sources := []model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference]{
		createFile(t, source0Data),
		createFile(t, source1Data),
		createFile(t, source2Data),
	}
	require.NotNil(t, sources[0].Message.GetList())
	require.NotNil(t, sources[1].Message.GetList())
	require.NotNil(t, sources[2].Message.GetHole())

	t.Run("ManySegments", func(t *testing.T) {
		// Compose a file out of many small slices, interleaved
		// with inline data and fills, similar to what a linker
		// would produce.
		var expected []byte
		b := &segmentListBuilder{}
		rng := rand.New(rand.NewPCG(3, 4))
		for range 5000 {
			switch rng.IntN(6) {
			case 0, 1, 2:
				input := rng.IntN(3)
				data := [][]byte{source0Data, source1Data, source2Data}[input]
				offset := rng.IntN(len(data))
				size := rng.IntN(min(len(data)-offset, 200)) + 1
				b.slice(uint64(input), uint64(offset), uint64(size))
				expected = append(expected, data[offset:][:size]...)
			case 3:
				data := make([]byte, rng.IntN(8)+1)
				for i := range data {
					data[i] = byte(rng.Uint32())
				}
				b.inline(data)
				expected = append(expected, data...)
			case 4:
				size := rng.IntN(100) + 1
				b.fill(0, uint64(size))
				expected = append(expected, make([]byte, size)...)
			case 5:
				size := rng.IntN(100) + 1
				b.fill(0x90, uint64(size))
				expected = append(expected, bytes.Repeat([]byte{0x90}, size)...)
			}
		}
		// Contiguous slices of the same source should be merged.
		b.slice(0, 1000, 10).slice(0, 1010, 10)
		expected = append(expected, source0Data[1000:1020]...)

		fileContents, err := compose(b.build(3), sources)
		require.NoError(t, err)
		require.NotNil(t, fileContents.Message.GetList())
		requireContents(t, expected, fileContents)

		// The composed file consists entirely of data, except
		// for the fills of null bytes and slices of the source
		// file consisting of null bytes.
		entry := util.Must(model_filesystem.NewFileContentsEntryFromProto(fileContents))
		offset, err := fileReader.FileSeek(ctx, entry, 0, filesystem.Hole)
		require.NoError(t, err)
		require.Less(t, *offset, uint64(len(expected)))
		require.Equal(t, byte(0), expected[*offset])
	})

	t.Run("LargeInline", func(t *testing.T) {
		// Inline data and fills that are too large to be stored
		// inline should be stored as chunks.
		data := bytes.Repeat([]byte("0123456789"), 20000)
		fileContents, err := compose((&segmentListBuilder{}).inline(data).fill(0x41, 300000).slice(1, 5, 10).build(3), sources)
		require.NoError(t, err)
		requireContents(t, append(append(append([]byte(nil), data...), bytes.Repeat([]byte{0x41}, 300000)...), source1Data[5:15]...), fileContents)
	})

	t.Run("SingleSliceOfEntireSource", func(t *testing.T) {
		// A file consisting of a single slice covering an entire
		// source file is identical to the source file.
		fileContents, err := compose((&segmentListBuilder{}).slice(1, 0, uint64(len(source1Data))).build(3), sources)
		require.NoError(t, err)
		require.Equal(t, sources[1].Message.GetList().GetReference().GetReference().GetIndex(), fileContents.Message.GetList().GetReference().GetReference().GetIndex())
		require.Equal(t, sources[1].Message.TotalSizeBytes, fileContents.Message.TotalSizeBytes)
		requireContents(t, source1Data, fileContents)
	})

	t.Run("SingleSliceOfPartOfSource", func(t *testing.T) {
		fileContents, err := compose((&segmentListBuilder{}).slice(0, 7, 1000).build(3), sources)
		require.NoError(t, err)
		requireContents(t, source0Data[7:1007], fileContents)
	})

	t.Run("SingleInline", func(t *testing.T) {
		fileContents, err := compose((&segmentListBuilder{}).inline([]byte("Hello")).build(3), sources)
		require.NoError(t, err)
		require.NotNil(t, fileContents.Message.GetChunkReference())
		requireContents(t, []byte("Hello"), fileContents)
	})

	t.Run("OnlyNullBytes", func(t *testing.T) {
		fileContents, err := compose((&segmentListBuilder{}).fill(0, 10).slice(2, 0, 100).build(3), sources)
		require.NoError(t, err)
		require.NotNil(t, fileContents.Message.GetHole())
		require.Equal(t, uint64(110), fileContents.Message.TotalSizeBytes)
	})

	t.Run("Empty", func(t *testing.T) {
		fileContents, err := compose((&segmentListBuilder{}).build(3), sources)
		require.NoError(t, err)
		require.Nil(t, fileContents.Message)
	})

	t.Run("SliceOutOfBounds", func(t *testing.T) {
		_, err := compose((&segmentListBuilder{}).slice(0, 99990, 11).build(3), sources)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("WrongNumberOfSources", func(t *testing.T) {
		_, err := compose((&segmentListBuilder{}).slice(0, 0, 1).build(2), sources)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("SliceOfComposedFile", func(t *testing.T) {
		// Slices may not refer to files that contain slices
		// themselves. This is detected when reading.
		composed, err := compose((&segmentListBuilder{}).slice(0, 0, 3000).inline([]byte("x")).slice(1, 0, 3000).build(3), sources)
		require.NoError(t, err)
		fileContents, err := compose((&segmentListBuilder{}).slice(0, 0, 10).slice(1, 0, 6001).build(2), []model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference]{sources[0], composed})
		require.NoError(t, err)
		entry := util.Must(model_filesystem.NewFileContentsEntryFromProto(fileContents))
		_, err = fileReader.FileReadAll(ctx, entry, 1<<20)
		require.Equal(t, codes.InvalidArgument, status.Code(err), err)
	})

	t.Run("SliceAtTopLevel", func(t *testing.T) {
		// Slices and inline data may only be stored in lists.
		_, err := model_filesystem.NewFileContentsEntryFromProto(model_core.NewSimpleMessage[object.LocalReference](&model_filesystem_pb.FileContents{
			Level:          &model_filesystem_pb.FileContents_Inline{Inline: []byte("Hello")},
			TotalSizeBytes: 5,
		}))
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}
