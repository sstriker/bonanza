package help

import (
	"bytes"
	"testing"

	"bonanza.build/pkg/bazelclient/arguments"

	"github.com/stretchr/testify/require"
)

func TestWriteHelp(t *testing.T) {
	t.Run("ListCommands", func(t *testing.T) {
		var b bytes.Buffer
		require.NoError(t, writeHelp(&b, nil, arguments.HelpVerbosity_Medium))
		require.Contains(t, b.String(), "Available commands:\n")
		require.Contains(t, b.String(), "\n  build\n")
		require.Contains(t, b.String(), "\n  query\n")
	})

	t.Run("CommandFlags", func(t *testing.T) {
		// Flags inherited from ancestor commands are included.
		var b bytes.Buffer
		require.NoError(t, writeHelp(&b, []string{"build"}, arguments.HelpVerbosity_Short))
		require.Contains(t, b.String(), "Options for build:\n")
		require.Contains(t, b.String(), "\n  --keep_going [-k]\n")
		require.Contains(t, b.String(), "\n  --color\n")
		require.NotContains(t, b.String(), "Continue as much as possible")

		b.Reset()
		require.NoError(t, writeHelp(&b, []string{"build"}, arguments.HelpVerbosity_Long))
		require.Contains(t, b.String(), "    Continue as much as possible after an error.")
	})

	t.Run("UnknownCommand", func(t *testing.T) {
		var b bytes.Buffer
		require.EqualError(t, writeHelp(&b, []string{"nonexistent"}, arguments.HelpVerbosity_Medium), "ERROR: \"nonexistent\" is not a known command")
	})
}
