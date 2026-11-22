package analysis

import (
	"testing"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/stretchr/testify/require"
)

func TestInferArchiveFormatFromURL(t *testing.T) {
	for url, expectedFormat := range map[string]model_analysis_pb.HttpArchiveContents_Key_Format{
		"https://example.com/archive.zip":     model_analysis_pb.HttpArchiveContents_Key_ZIP,
		"https://example.com/library.jar":     model_analysis_pb.HttpArchiveContents_Key_ZIP,
		"https://example.com/package.whl":     model_analysis_pb.HttpArchiveContents_Key_ZIP,
		"https://example.com/archive.tar":     model_analysis_pb.HttpArchiveContents_Key_TAR,
		"https://example.com/archive.tar.gz":  model_analysis_pb.HttpArchiveContents_Key_TAR_GZ,
		"https://example.com/archive.tgz":     model_analysis_pb.HttpArchiveContents_Key_TAR_GZ,
		"https://example.com/archive.tar.xz":  model_analysis_pb.HttpArchiveContents_Key_TAR_XZ,
		"https://example.com/archive.txz":     model_analysis_pb.HttpArchiveContents_Key_TAR_XZ,
		"https://example.com/archive.tar.zst": model_analysis_pb.HttpArchiveContents_Key_TAR_ZST,
		"https://example.com/archive.tzst":    model_analysis_pb.HttpArchiveContents_Key_TAR_ZST,
		"https://example.com/archive.tar.bz2": model_analysis_pb.HttpArchiveContents_Key_TAR_BZ2,
		"https://example.com/archive.tbz":     model_analysis_pb.HttpArchiveContents_Key_TAR_BZ2,
		".tar.gz":                             model_analysis_pb.HttpArchiveContents_Key_TAR_GZ,
	} {
		t.Run(url, func(t *testing.T) {
			format, ok := inferArchiveFormatFromURL(url)
			require.True(t, ok)
			require.Equal(t, expectedFormat, format)
		})
	}

	t.Run("Unknown", func(t *testing.T) {
		_, ok := inferArchiveFormatFromURL("https://example.com/archive.rar")
		require.False(t, ok)
	})
}
