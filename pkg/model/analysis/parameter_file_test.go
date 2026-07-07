package analysis

import (
	"strings"
	"testing"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/stretchr/testify/require"
)

// Expected values correspond to the ones in Bazel's ShellEscaperTest,
// as parameter files rendered with the "shell" format need to be byte
// for byte identical to the ones written by Bazel.
func TestBazelShellEscape(t *testing.T) {
	for input, expected := range map[string]string{
		"":                                     "''",
		"foo":                                  "foo",
		"@bar":                                 "@bar",
		"foo bar":                              "'foo bar'",
		"'foo'":                                `''\''foo'\'''`,
		`\'foo\'`:                              `'\'\''foo\'\'''`,
		"${filename%.c}.o":                     "'${filename%.c}.o'",
		"<html!>":                              "'<html!>'",
		"~not_home":                            "'~not_home'",
		"baz'qux":                              `'baz'\''qux'`,
		"$BAZ":                                 "'$BAZ'",
		`"quot"`:                               `'"quot"'`,
		`\`:                                    `'\'`,
		"external/protobuf+3.19.6/src/goo~gle": "external/protobuf+3.19.6/src/goo~gle",
		"external/+install_dev_dependencies+foo/pkg": "external/+install_dev_dependencies+foo/pkg",
	} {
		require.Equal(t, expected, bazelShellEscape(input), "input %#v", input)
	}
}

func TestAppendParamFileArgument(t *testing.T) {
	t.Run("Multiline", func(t *testing.T) {
		var w strings.Builder
		for _, argument := range []string{"--foo", "hello world", ""} {
			require.NoError(t, appendParamFileArgument(&w, model_analysis_pb.Args_Leaf_UseParamFile_MULTILINE, argument))
		}
		require.Equal(t, "--foo\nhello world\n\n", w.String())
	})

	t.Run("Shell", func(t *testing.T) {
		var w strings.Builder
		for _, argument := range []string{"--foo", "hello world", ""} {
			require.NoError(t, appendParamFileArgument(&w, model_analysis_pb.Args_Leaf_UseParamFile_SHELL, argument))
		}
		require.Equal(t, "--foo\n'hello world'\n''\n", w.String())
	})

	t.Run("FlagPerLine", func(t *testing.T) {
		// Rendering of the "flag_per_line" format is not
		// implemented, which callers report as content None.
		var w strings.Builder
		require.Error(t, appendParamFileArgument(&w, model_analysis_pb.Args_Leaf_UseParamFile_FLAG_PER_LINE, "--foo"))
	})
}
