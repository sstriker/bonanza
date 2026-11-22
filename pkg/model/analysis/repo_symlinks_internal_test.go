package analysis

import (
	"strings"
	"testing"

	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"
)

func TestChangeTrackingDirectoryExistingFileResolverSymlinks(t *testing.T) {
	type (
		testDirectory = changeTrackingDirectory[object.LocalReference, model_core.ReferenceMetadata]
		testFile      = changeTrackingFile[object.LocalReference, model_core.ReferenceMetadata]
		testResolver  = changeTrackingDirectoryExistingFileResolver[object.LocalReference, model_core.ReferenceMetadata]
	)
	component := path.MustNewComponent

	// Create the following input root, placed at /root:
	//
	//	dir/file
	//	relative_dir_link -> dir
	//	absolute_dir_link -> /root/dir
	//	file_link -> dir/file
	//	escaping_link -> /etc/passwd
	//	loop -> loop
	file := &testFile{}
	dir := &testDirectory{}
	dir.setFileSimple(component("file"), file)
	root := &testDirectory{}
	root.setDirectorySimple(component("dir"), dir)
	for name, target := range map[string]string{
		"relative_dir_link": "dir",
		"absolute_dir_link": "/root/dir",
		"file_link":         "dir/file",
		"escaping_link":     "/etc/passwd",
		"loop":              "loop",
	} {
		root.setSymlinkSimple(component(name), path.UNIXFormat.NewParser(target))
	}
	virtualRootScopeWalkerFactory, err := path.NewVirtualRootScopeWalkerFactory(path.UNIXFormat.NewParser("/root"), nil)
	require.NoError(t, err)

	resolve := func(p string, followTerminalSymlink bool) (*testResolver, error) {
		r := &testResolver{
			stack:                 util.NewNonEmptyStack(root),
			followTerminalSymlink: followTerminalSymlink,
		}
		err := path.Resolve(path.UNIXFormat.NewParser(p), path.NewLoopDetectingScopeWalker(virtualRootScopeWalkerFactory.New(r)))
		return r, err
	}
	resolvedPath := func(r *testResolver) string {
		components := make([]string, 0, len(r.components)+1)
		for _, c := range r.components {
			components = append(components, c.String())
		}
		if r.TerminalName != nil {
			components = append(components, r.TerminalName.String())
		}
		return strings.Join(components, "/")
	}

	t.Run("IntermediateSymlinks", func(t *testing.T) {
		// Symbolic links in directory components are always
		// followed, regardless of whether they are relative or
		// absolute.
		for _, p := range []string{"/root/relative_dir_link/file", "/root/absolute_dir_link/file"} {
			r, err := resolve(p, false)
			require.NoError(t, err)
			require.True(t, r.gotScope)
			require.NoError(t, r.checkSymlinksStayInInputRoot(nil))
			require.Equal(t, "dir/file", resolvedPath(r))
			f, err := r.getFile()
			require.NoError(t, err)
			require.Same(t, file, f)
		}
	})

	t.Run("TerminalSymlinkNotFollowed", func(t *testing.T) {
		// Operations like repository_ctx.delete() should act on
		// the symbolic link itself.
		r, err := resolve("/root/file_link", false)
		require.NoError(t, err)
		require.True(t, r.gotScope)
		require.Equal(t, "file_link", resolvedPath(r))
	})

	t.Run("TerminalSymlinkFollowed", func(t *testing.T) {
		r, err := resolve("/root/file_link", true)
		require.NoError(t, err)
		require.True(t, r.gotScope)
		require.Equal(t, "dir/file", resolvedPath(r))
		f, err := r.getFile()
		require.NoError(t, err)
		require.Same(t, file, f)
	})

	t.Run("EscapingSymlink", func(t *testing.T) {
		r, err := resolve("/root/escaping_link", true)
		require.NoError(t, err)
		require.False(t, r.gotScope)
		require.True(t, r.followedSymlink)
	})

	t.Run("PathOutsideInputRoot", func(t *testing.T) {
		// Paths outside the input root that don't traverse any
		// symbolic links in the input root are left alone, so
		// that they can be accessed on the host.
		r, err := resolve("/usr/bin/env", true)
		require.NoError(t, err)
		require.False(t, r.gotScope)
		require.False(t, r.followedSymlink)
	})

	t.Run("Loop", func(t *testing.T) {
		_, err := resolve("/root/loop", true)
		require.Error(t, err)
	})
}
