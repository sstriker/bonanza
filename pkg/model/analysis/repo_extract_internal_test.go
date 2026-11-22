package analysis

import (
	"testing"

	model_core "bonanza.build/pkg/model/core"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

func TestChangeTrackingDirectoryOverlayArchive(t *testing.T) {
	type (
		testDirectory = changeTrackingDirectory[object.LocalReference, model_core.ReferenceMetadata]
		testFile      = changeTrackingFile[object.LocalReference, model_core.ReferenceMetadata]
	)
	newContents := func(sizeBytes uint64) model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference] {
		return model_core.NewSimpleMessage[object.LocalReference](&model_filesystem_pb.FileContents{
			TotalSizeBytes: sizeBytes,
		})
	}
	component := path.MustNewComponent

	// Contents of the archive, as returned by extractArchive().
	archive := &archiveDirectory[object.LocalReference]{
		directories: map[path.Component]*archiveDirectory[object.LocalReference]{
			component("lib"): {
				files: map[path.Component]archiveFile[object.LocalReference]{
					component("new.txt"): {contents: newContents(1)},
				},
			},
		},
		files: map[path.Component]archiveFile[object.LocalReference]{
			component("tool.sh"):  {contents: newContents(2), isExecutable: true},
			component("conflict"): {contents: newContents(3)},
		},
		symlinks: map[path.Component]path.Parser{
			component("link"): path.UNIXFormat.NewParser("tool.sh"),
		},
	}

	// Existing contents of the output directory, which contains a
	// BUILD file written before extraction, and entries that
	// collide with ones in the archive.
	existingBuildFile := &testFile{contents: unmodifiedFileContents[object.LocalReference, model_core.ReferenceMetadata]{contents: newContents(4)}}
	existingLibFile := &testFile{contents: unmodifiedFileContents[object.LocalReference, model_core.ReferenceMetadata]{contents: newContents(5)}}
	output := &testDirectory{}
	output.setFileSimple(component("BUILD.bazel"), existingBuildFile)
	output.setDirectorySimple(component("conflict"), &testDirectory{})
	existingLib := &testDirectory{}
	existingLib.setFileSimple(component("old.txt"), existingLibFile)
	output.setDirectorySimple(component("lib"), existingLib)

	require.NoError(t, output.overlayDirectory(
		newChangeTrackingDirectoryFromArchive[object.LocalReference, model_core.ReferenceMetadata](archive),
		nil,
	))

	// Files not contained in the archive are preserved, while
	// directories present on both sides are merged.
	require.Same(t, existingBuildFile, output.files[component("BUILD.bazel")])
	require.Same(t, existingLibFile, output.directories[component("lib")].files[component("old.txt")])
	require.Contains(t, output.directories[component("lib")].files, component("new.txt"))

	// Entries from the archive replace existing ones of a different
	// type.
	require.NotContains(t, output.directories, component("conflict"))
	require.Contains(t, output.files, component("conflict"))
	require.True(t, output.files[component("tool.sh")].isExecutable)
	require.Equal(t, "tool.sh", mustGetUNIXString(t, output.symlinks[component("link")]))
}

func mustGetUNIXString(t *testing.T, p path.Parser) string {
	builder, scopeWalker := path.EmptyBuilder.Join(path.VoidScopeWalker)
	require.NoError(t, path.Resolve(p, scopeWalker))
	return builder.GetUNIXString()
}
