package analysis

import (
	"strings"
	"testing"

	model_core "bonanza.build/pkg/model/core"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

// newTrace builds a *path.Trace from a slash separated relative path.
// An empty string yields the nil trace (path ".").
func newTrace(s string) *path.Trace {
	var t *path.Trace
	if s == "" {
		return t
	}
	for _, component := range strings.Split(s, "/") {
		t = t.Append(path.MustNewComponent(component))
	}
	return t
}

// TestResolveHostPath exercises the symlink target normalization that
// underpins RepoPlatformHostPath's capture of host directories such as
// the local JDK. Symlinks whose targets escape the captured root
// directory must be rewritten to a clean absolute path on the host file
// system, anchoring relative targets at the symlink's own directory and
// collapsing ".." components. This is what allows those targets to be
// subsequently resolved through the virtual root without ascending above
// the captured root directory (which used to fail with "path resolves to
// a location above the root directory").
func TestResolveHostPath(t *testing.T) {
	sr := &changeTrackingDirectorySymlinksRelativizer[model_core.CreatedObjectTree, model_core.CreatedObjectTree]{
		rootPath: path.UNIXFormat.NewParser("/usr/lib/jvm/java-21-openjdk-amd64"),
	}

	for _, tc := range []struct {
		name     string
		dPath    string
		target   string
		expected string
	}{
		{
			// docs -> ../../../share/doc/openjdk-21-jre-headless,
			// a symlink at the captured root that escapes it
			// immediately. This is the case that regressed the
			// local_jdk scan.
			name:     "RelativeEscapeFromRoot",
			dPath:    "",
			target:   "../../../share/doc/openjdk-21-jre-headless",
			expected: "/usr/share/doc/openjdk-21-jre-headless",
		},
		{
			// lib/src.zip -> ../../openjdk-21/src.zip, escaping to
			// a sibling of the captured root.
			name:     "RelativeEscapeToSibling",
			dPath:    "lib",
			target:   "../../openjdk-21/src.zip",
			expected: "/usr/lib/jvm/openjdk-21/src.zip",
		},
		{
			// lib/libatk-wrapper.so -> ../../../x86_64-linux-gnu/jni/libatk-wrapper.so.
			name:     "RelativeEscapeAboveRoot",
			dPath:    "lib",
			target:   "../../../x86_64-linux-gnu/jni/libatk-wrapper.so",
			expected: "/usr/lib/x86_64-linux-gnu/jni/libatk-wrapper.so",
		},
		{
			// An absolute target is taken as-is, regardless of the
			// symlink's own location.
			name:     "AbsoluteTarget",
			dPath:    "lib/security",
			target:   "/etc/ssl/certs/java/cacerts",
			expected: "/etc/ssl/certs/java/cacerts",
		},
		{
			// A relative target that climbs out of the captured
			// root only to descend straight back into it must
			// normalize to a location inside the captured root.
			name:     "RelativeReentersRoot",
			dPath:    "lib",
			target:   "../../java-21-openjdk-amd64/release",
			expected: "/usr/lib/jvm/java-21-openjdk-amd64/release",
		},
		{
			// Excess ".." components at the file system root are
			// dropped rather than producing "/..".
			name:     "EscapesFileSystemRoot",
			dPath:    "",
			target:   "../../../../../../../../etc/passwd",
			expected: "/etc/passwd",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := sr.resolveHostPath(
				newTrace(tc.dPath),
				path.UNIXFormat.NewParser(tc.target),
			)
			require.NoError(t, err)
			require.Equal(t, tc.expected, resolved.(path.Stringer).GetUNIXString())
		})
	}
}
