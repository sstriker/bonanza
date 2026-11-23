package filesystem

import (
	"context"
	"io"
	"sort"

	model_core "bonanza.build/pkg/model/core"
	model_encoding "bonanza.build/pkg/model/encoding"
	model_parser "bonanza.build/pkg/model/parser"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/util"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FileReader is a helper type for reading the contents of files that
// have been written to storage, using traditional interfaces like
// io.Reader, io.ReaderAt, io.Writer, etc.
type FileReader[TReference object.BasicReference] struct {
	fileContentsListReader     model_parser.DecodingObjectReader[TReference, FileContentsList[TReference]]
	fileChunkReader            model_parser.DecodingObjectReader[TReference, []byte]
	fileChunkReaderConcurrency *semaphore.Weighted
	fileSliceReaderConcurrency *semaphore.Weighted
}

// fileSliceReaderConcurrency is the maximum number of slices of other
// files that are read in parallel. Slices are typically small, meaning
// that reading them is dominated by the latency of reading the chunks
// of source files.
const fileSliceReaderConcurrency = 64

// NewFileReader creates a FileReader that uses the specified object
// readers for chunks and file contents lists.
func NewFileReader[TReference object.BasicReference](
	fileContentsListReader model_parser.DecodingObjectReader[TReference, FileContentsList[TReference]],
	fileChunkReader model_parser.DecodingObjectReader[TReference, []byte],
	fileChunkReaderConcurrency *semaphore.Weighted,
) *FileReader[TReference] {
	return &FileReader[TReference]{
		fileContentsListReader:     fileContentsListReader,
		fileChunkReader:            fileChunkReader,
		fileChunkReaderConcurrency: fileChunkReaderConcurrency,
		fileSliceReaderConcurrency: semaphore.NewWeighted(fileSliceReaderConcurrency),
	}
}

// NewFileReaderFromProto creates a FileReader that is capable of
// reading files whose objects are encoded according to the file access
// parameters stored in a Protobuf message. Objects are parsed through
// the provided ParsedObjectPoolIngester, so that they may be cached.
func NewFileReaderFromProto[TReference object.BasicReference](
	m *model_filesystem_pb.FileAccessParameters,
	referenceFormat object.ReferenceFormat,
	binaryEncoderFactory model_encoding.BinaryEncoderFactory,
	parsedObjectPoolIngester *model_parser.ParsedObjectPoolIngester[TReference],
	fileChunkReaderConcurrency *semaphore.Weighted,
) (*FileReader[TReference], error) {
	fileAccessParameters, err := NewFileAccessParametersFromProto(m, referenceFormat, binaryEncoderFactory)
	if err != nil {
		return nil, err
	}
	return NewFileReader(
		model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[TReference](fileAccessParameters.GetFileContentsListEncoder()),
				NewFileContentsListObjectParser[TReference](),
			),
		),
		model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[TReference](fileAccessParameters.GetChunkEncoder()),
				model_parser.NewRawObjectParser[TReference](),
			),
		),
		fileChunkReaderConcurrency,
	), nil
}

// GetDecodingParametersSizeBytes returns the expected size of the
// decoding parameters that a reference to a file contents list or
// chunks should have.
func (fr *FileReader[TReference]) GetDecodingParametersSizeBytes(isFileContentsList bool) int {
	if isFileContentsList {
		return fr.fileContentsListReader.GetDecodingParametersSizeBytes()
	}
	return fr.fileChunkReader.GetDecodingParametersSizeBytes()
}

// fileReaderPart is a part of a file that is returned by
// getNextPart(). It either corresponds to a hole, a chunk, a slice of
// another file, or inline data.
type fileReaderPart[TReference object.BasicReference] struct {
	entry       *FileContentsEntry[TReference]
	offsetBytes uint64
	sizeBytes   uint64
}

func (fr *FileReader[TReference]) getNextPart(ctx context.Context, fileContentsIterator *FileContentsIterator[TReference]) (fileReaderPart[TReference], error) {
	for {
		entry, partOffsetBytes, partSizeBytes := fileContentsIterator.getCurrentEntry()
		partReference := entry.GetReference()
		if partReference == nil || partReference.Value.GetDegree() == 0 {
			// Reached a hole, chunk, slice or inline data.
			fileContentsIterator.ToNextPart()
			return fileReaderPart[TReference]{
				entry:       entry,
				offsetBytes: partOffsetBytes,
				sizeBytes:   partSizeBytes,
			}, nil
		}

		// We need to push one or more file contents lists onto
		// the stack to reach a chunk.
		fileContentsList, err := fr.fileContentsListReader.ReadObject(ctx, *partReference)
		if err != nil {
			return fileReaderPart[TReference]{}, util.StatusWrapf(err, "Failed to read file contents list with reference %s", model_core.DecodableLocalReferenceToString(*partReference))
		}
		if err := fileContentsIterator.PushFileContentsList(fileContentsList); err != nil {
			return fileReaderPart[TReference]{}, util.StatusWrapf(err, "Invalid file contents list with reference %s", model_core.DecodableLocalReferenceToString(*partReference))
		}
	}
}

// readPart reads the data of a part that is not a hole, starting at the
// offset within the part. At most maximumSizeBytes bytes are returned.
func (fr *FileReader[TReference]) readPart(ctx context.Context, part fileReaderPart[TReference], maximumSizeBytes uint64) ([]byte, error) {
	if slice := part.entry.slice; slice != nil {
		readSizeBytes := min(maximumSizeBytes, part.sizeBytes-part.offsetBytes)
		if part.offsetBytes >= slice.sourceSizeBytes {
			// Only the suffix needs to be read.
			return slice.suffix[part.offsetBytes-slice.sourceSizeBytes:][:readSizeBytes], nil
		}

		data := make([]byte, readSizeBytes)
		sourceReadSizeBytes := min(readSizeBytes, slice.sourceSizeBytes-part.offsetBytes)
		if err := fr.readSliceSource(ctx, slice, part.offsetBytes, data[:sourceReadSizeBytes]); err != nil {
			return nil, util.StatusWrap(err, "Failed to read slice")
		}
		copy(data[sourceReadSizeBytes:], slice.suffix)
		return data, nil
	}

	chunk, err := fr.readChunk(ctx, *part.entry.GetReference(), part.offsetBytes, part.sizeBytes)
	if err != nil {
		return nil, err
	}
	return chunk[:min(maximumSizeBytes, uint64(len(chunk)))], nil
}

// readSliceSource reads data from the source file of a slice. As
// slices are typically small and only span one or two chunks, chunks
// are read sequentially. Parallelism is achieved by reading multiple
// slices concurrently.
func (fr *FileReader[TReference]) readSliceSource(ctx context.Context, slice *slicePart[TReference], offsetBytes uint64, output []byte) error {
	fileContentsIterator := NewFileContentsIterator(slice.source, slice.sourceOffsetBytes+offsetBytes)
	for len(output) > 0 {
		part, err := fr.getNextPart(ctx, &fileContentsIterator)
		if err != nil {
			return err
		}
		readSizeBytes := min(uint64(len(output)), part.sizeBytes-part.offsetBytes)
		if part.entry.IsHole() {
			clear(output[:readSizeBytes])
		} else if part.entry.slice != nil {
			// Slices may not refer to files that contain
			// slices themselves, as that would permit
			// constructing chains of slices of arbitrary
			// length.
			return status.Error(codes.InvalidArgument, "Slice source contains slices")
		} else {
			chunk, err := fr.readChunk(ctx, *part.entry.GetReference(), part.offsetBytes, part.sizeBytes)
			if err != nil {
				return err
			}
			copy(output, chunk[:readSizeBytes])
		}
		output = output[readSizeBytes:]
	}
	return nil
}

func (fr *FileReader[TReference]) readChunk(ctx context.Context, partReference model_core.Decodable[TReference], partOffsetBytes, partSizeBytes uint64) ([]byte, error) {
	chunk, err := fr.fileChunkReader.ReadObject(ctx, partReference)
	if err != nil {
		return nil, util.StatusWrapf(err, "Failed to read chunk with reference %s", model_core.DecodableLocalReferenceToString(partReference))
	}
	if uint64(len(chunk)) != partSizeBytes {
		return nil, status.Errorf(codes.InvalidArgument, "Chunk with reference %s is %d bytes in size, while %d bytes were expected", model_core.DecodableLocalReferenceToString(partReference), len(chunk), partSizeBytes)
	}
	return chunk[partOffsetBytes:], nil
}

// chunkAndHoleWriter is used by FileReader.walkChunksAndHoles() to
// report chunks and holes contained in a file. Due to contents of
// chunks being loaded in parallel, these methods can be called in
// random order.
type chunkAndHoleWriter interface {
	WriteChunk(offsetBytes uint64, data []byte) error
	WriteHole(offsetBytes, sizeBytes uint64) error
}

func (fr *FileReader[TReference]) walkChunksAndHoles(
	ctx context.Context,
	fileContentsIterator *FileContentsIterator[TReference],
	outputSizeBytes uint64,
	writer chunkAndHoleWriter,
) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		outputOffsetBytes := uint64(0)
		for outputOffsetBytes < outputSizeBytes {
			part, err := fr.getNextPart(ctx, fileContentsIterator)
			if err != nil {
				return err
			}

			readSizeBytes := min(outputSizeBytes-outputOffsetBytes, part.sizeBytes-part.offsetBytes)
			if part.entry.IsHole() {
				// Reached a hole.
				if err := writer.WriteHole(outputOffsetBytes, readSizeBytes); err != nil {
					return err
				}
			} else {
				// Reached a chunk or a slice of another
				// file. Slices are read using a separate
				// semaphore, as reading them requires
				// reading chunks of the source file.
				currentOutputOffsetBytes := outputOffsetBytes
				concurrency := fr.fileChunkReaderConcurrency
				if part.entry.slice != nil {
					concurrency = fr.fileSliceReaderConcurrency
				}
				if err := util.AcquireSemaphore(groupCtx, concurrency, 1); err != nil {
					return err
				}
				group.Go(func() error {
					defer concurrency.Release(1)

					data, err := fr.readPart(groupCtx, part, readSizeBytes)
					if err != nil {
						return err
					}
					return writer.WriteChunk(currentOutputOffsetBytes, data)
				})
			}
			outputOffsetBytes += readSizeBytes
		}
		return nil
	})
	return group.Wait()
}

// byteSliceChunkAndHoleWriter is an implementation of
// chunkAndHoleWriter that writes the contents of chunks and holes into
// a byte slice. It can be used to implement things like io.ReaderAt and
// io.ReadAll().
type byteSliceChunkAndHoleWriter struct {
	output []byte
}

func (w byteSliceChunkAndHoleWriter) WriteChunk(offsetBytes uint64, data []byte) error {
	copy(w.output[offsetBytes:], data)
	return nil
}

func (w byteSliceChunkAndHoleWriter) WriteHole(offsetBytes, sizeBytes uint64) error {
	clear(w.output[offsetBytes:][:sizeBytes])
	return nil
}

// FileReadAll reads the entire contents of a file that has been written
// to storage, returning the contents as a byte slice.
func (fr *FileReader[TReference]) FileReadAll(ctx context.Context, fileContents FileContentsEntry[TReference], maximumSizeBytes uint64) ([]byte, error) {
	endBytes := fileContents.GetEndBytes()
	if endBytes > maximumSizeBytes {
		return nil, status.Errorf(codes.InvalidArgument, "File is %d bytes in size, which exceeds the permitted maximum of %d bytes", endBytes, maximumSizeBytes)
	}
	p := make([]byte, endBytes)
	fileContentsIterator := NewFileContentsIterator(fileContents, 0)
	if err := fr.walkChunksAndHoles(
		ctx,
		&fileContentsIterator,
		endBytes,
		byteSliceChunkAndHoleWriter{
			output: p,
		},
	); err != nil {
		return nil, err
	}
	return p, nil
}

// FileReadAt performs a single read of a portion of a file that has
// been written to storage. The provided offset and output array MUST
// reside within the boundaries of the file.
func (fr *FileReader[TReference]) FileReadAt(ctx context.Context, fileContents FileContentsEntry[TReference], p []byte, offsetBytes uint64) (int, error) {
	fileContentsIterator := NewFileContentsIterator(fileContents, offsetBytes)
	if err := fr.walkChunksAndHoles(
		ctx,
		&fileContentsIterator,
		uint64(len(p)),
		byteSliceChunkAndHoleWriter{
			output: p,
		},
	); err != nil {
		return 0, err
	}
	return len(p), nil
}

// FileSeek returns the offset of the first byte at or after offsetBytes
// that belongs to a region of the provided type. This can be used to
// implement lseek()'s SEEK_DATA and SEEK_HOLE. The provided offset MUST
// be less than the size of the file.
//
// Files are considered to have an implicit hole at the end. This means
// that seeking for a hole always yields an offset. Seeking for data
// yields nil if the file only contains a hole at or after the provided
// offset.
//
// File contents lists that are not sparse consist entirely of data,
// meaning that they don't need to be read. The cost of this function
// is thus proportional to the number of holes in the file, as opposed
// to its size.
func (fr *FileReader[TReference]) FileSeek(ctx context.Context, fileContents FileContentsEntry[TReference], offsetBytes uint64, regionType filesystem.RegionType) (*uint64, error) {
	endBytes := fileContents.GetEndBytes()
	nextOffsetBytes, err := fr.seekInPart(ctx, &fileContents, 0, endBytes, offsetBytes, regionType)
	if err != nil {
		return nil, err
	}
	if nextOffsetBytes == nil && regionType == filesystem.Hole {
		return &endBytes, nil
	}
	return nextOffsetBytes, nil
}

// seekInPart is used by FileSeek() to search for a region of a given
// type within a single part of a file. The part spans the range
// [startBytes, endBytes) within the file, and searching starts at
// offsetBytes, which lies within this range. If no region of the
// requested type exists in the remainder of the part, nil is returned.
func (fr *FileReader[TReference]) seekInPart(ctx context.Context, part *FileContentsEntry[TReference], startBytes, endBytes, offsetBytes uint64, regionType filesystem.RegionType) (*uint64, error) {
	if part.slice != nil {
		// Part is a slice of another file.
		// Slices are considered to consist entirely of data,
		// even if the source file contains holes.
		if regionType == filesystem.Data {
			return &offsetBytes, nil
		}
		return nil, nil
	}
	reference := part.GetReference()
	if reference == nil {
		// Part is a hole.
		if regionType == filesystem.Hole {
			return &offsetBytes, nil
		}
		return nil, nil
	}
	if reference.Value.GetHeight() == 0 || part.contiguousCount > 0 {
		// Part is a chunk, or a file contents list that does
		// not contain any holes. Both consist entirely of data.
		if regionType == filesystem.Data {
			return &offsetBytes, nil
		}
		return nil, nil
	}

	// Part is a file contents list containing one or more holes.
	// Descend into it, starting at the part containing the offset.
	list, err := fr.fileContentsListReader.ReadObject(ctx, *reference)
	if err != nil {
		return nil, util.StatusWrapf(err, "Failed to read file contents list with reference %s", model_core.DecodableLocalReferenceToString(*reference))
	}
	if actualSizeBytes, expectedSizeBytes := list[len(list)-1].GetEndBytes(), endBytes-startBytes; actualSizeBytes != expectedSizeBytes {
		return nil, status.Errorf(codes.InvalidArgument, "Parts in the file contents list with reference %s have a total size of %d bytes, while %d bytes were expected", model_core.DecodableLocalReferenceToString(*reference), actualSizeBytes, expectedSizeBytes)
	}
	relativeOffsetBytes := offsetBytes - startBytes
	for i := sort.Search(len(list), func(i int) bool {
		return relativeOffsetBytes < list[i].GetEndBytes()
	}); i < len(list); {
		childPart := &list[i]
		if regionType == filesystem.Hole && childPart.contiguousCount > 0 {
			// This part and the ones that follow it don't
			// contain any holes. Skip to the next part that
			// may contain a hole.
			i += childPart.contiguousCount
			continue
		}
		childStartBytes := startBytes
		if i > 0 {
			childStartBytes += list[i-1].GetEndBytes()
		}
		nextOffsetBytes, err := fr.seekInPart(
			ctx,
			childPart,
			childStartBytes,
			startBytes+childPart.GetEndBytes(),
			max(offsetBytes, childStartBytes),
			regionType,
		)
		if err != nil || nextOffsetBytes != nil {
			return nextOffsetBytes, err
		}
		i++
	}
	return nil, nil
}

// FileOpenRead opens a file that has been written to storage, returning
// a handle that allows the file to be read sequentially.
func (fr *FileReader[TReference]) FileOpenRead(ctx context.Context, fileContents FileContentsEntry[TReference], offsetBytes uint64) *SequentialFileReader[TReference] {
	return &SequentialFileReader[TReference]{
		context:              ctx,
		fileReader:           fr,
		fileContentsIterator: NewFileContentsIterator(fileContents, offsetBytes),
		offsetBytes:          offsetBytes,
		sizeBytes:            fileContents.GetEndBytes(),
	}
}

// FileOpenReadAt opens a file that has been written to storage,
// returning an io.ReaderAt so that the file can be read at random.
func (fr *FileReader[TReference]) FileOpenReadAt(ctx context.Context, fileContents FileContentsEntry[TReference]) io.ReaderAt {
	return &randomAccessFileReader[TReference]{
		context:      ctx,
		fileReader:   fr,
		fileContents: fileContents,
	}
}

type fileChunkAndHoleWriter struct {
	file io.WriterAt
}

func (w fileChunkAndHoleWriter) WriteChunk(offsetBytes uint64, data []byte) error {
	_, err := w.file.WriteAt(data, int64(offsetBytes))
	return err
}

func (fileChunkAndHoleWriter) WriteHole(offsetBytes, sizeBytes uint64) error {
	return nil
}

// FileWriteTo writes the contents of a file residing in the Object
// Store to a local file. It is assumed that the file already has the
// desired size and consists of a single hole.
func (fr *FileReader[TReference]) FileWriteTo(ctx context.Context, fileContents FileContentsEntry[TReference], file io.WriterAt) error {
	fileContentsIterator := NewFileContentsIterator(fileContents, 0)
	return fr.walkChunksAndHoles(
		ctx,
		&fileContentsIterator,
		fileContents.GetEndBytes(),
		fileChunkAndHoleWriter{
			file: file,
		},
	)
}

// SequentialFileReader can be used to sequentially read data from a
// file that has been written to storage. SequentialFileReader buffers
// the last read chunk of data, meaning that small reads will not
// trigger redundant reads against the underlying storage backend.
type SequentialFileReader[TReference object.BasicReference] struct {
	context              context.Context
	fileReader           *FileReader[TReference]
	fileContentsIterator FileContentsIterator[TReference]
	chunk                []byte
	holeSizeBytes        uint64
	offsetBytes          uint64
	sizeBytes            uint64
}

var (
	_ io.Reader     = (*SequentialFileReader[object.LocalReference])(nil)
	_ io.ByteReader = (*SequentialFileReader[object.LocalReference])(nil)
)

// Read data from a file that has been written to the Object Store.
func (r *SequentialFileReader[TReference]) Read(p []byte) (int, error) {
	nRead := 0
	for {
		// Copy data from a previously read chunk.
		nChunk := copy(p, r.chunk)
		p = p[nChunk:]
		r.chunk = r.chunk[nChunk:]
		nRead += nChunk

		// Clear data from a previously observed hole.
		nHole := int(min(r.holeSizeBytes, uint64(len(p))))
		clear(p[:nHole])
		p = p[nHole:]
		r.holeSizeBytes -= uint64(nHole)
		nRead += nHole

		// Read the next chunk if we still have space in the
		// buffer or are not at end of file.
		if len(p) == 0 {
			return nRead, nil
		}
		if r.offsetBytes >= r.sizeBytes {
			return nRead, io.EOF
		}

		part, err := r.fileReader.getNextPart(r.context, &r.fileContentsIterator)
		if err != nil {
			return nRead, err
		}
		if part.entry.IsHole() {
			r.holeSizeBytes = part.sizeBytes - part.offsetBytes
			r.offsetBytes += r.holeSizeBytes
		} else {
			chunk, err := r.fileReader.readPart(r.context, part, part.sizeBytes-part.offsetBytes)
			if err != nil {
				return nRead, err
			}
			r.chunk = chunk
			r.offsetBytes += uint64(len(chunk))
		}
	}
}

// ReadByte reads a single byte of data from a file that has been
// written to the Object Store.
func (r *SequentialFileReader[TReference]) ReadByte() (byte, error) {
	var b [1]byte
	if n, err := r.Read(b[:]); n == 0 {
		return 0, err
	}
	return b[0], nil
}

type randomAccessFileReader[TReference object.BasicReference] struct {
	context      context.Context
	fileReader   *FileReader[TReference]
	fileContents FileContentsEntry[TReference]
}

func (r *randomAccessFileReader[TReference]) ReadAt(p []byte, offsetBytes int64) (int, error) {
	// Limit the read operation to the size of the file.
	endBytes := r.fileContents.GetEndBytes()
	if uint64(offsetBytes) > endBytes {
		return 0, io.EOF
	}
	remainingBytes := endBytes - uint64(offsetBytes)
	if uint64(len(p)) > remainingBytes {
		p = p[:remainingBytes]
	}

	return r.fileReader.FileReadAt(r.context, r.fileContents, p, uint64(offsetBytes))
}

type (
	// FileContentsListReaderForTesting is used for generating mocks
	// that are used by FileReader's unit tests.
	FileContentsListReaderForTesting = model_parser.DecodingObjectReader[object.LocalReference, FileContentsList[object.LocalReference]]
	// FileChunkReaderForTesting is used for generating mocks that
	// are used by FileReader's unit tests.
	FileChunkReaderForTesting = model_parser.DecodingObjectReader[object.LocalReference, []byte]
)
