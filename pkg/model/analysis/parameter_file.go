package analysis

import (
	"errors"
	"strings"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
)

// appendParamFileArgument renders a single argument the way Bazel's
// ParameterFile.writeParamFile() does: the argument, shell-quoted if
// the SHELL format is used, followed by a newline character. The
// newline is also emitted after the last argument.
func appendParamFileArgument(w *strings.Builder, format model_analysis_pb.Args_Leaf_UseParamFile_Format, argument string) error {
	switch format {
	case model_analysis_pb.Args_Leaf_UseParamFile_MULTILINE:
		w.WriteString(argument)
	case model_analysis_pb.Args_Leaf_UseParamFile_SHELL:
		w.WriteString(bazelShellEscape(argument))
	default:
		return errors.New("unknown parameter file format")
	}
	w.WriteByte('\n')
	return nil
}

// bazelShellEscape escapes a string by adding strong (single) quotes
// around it if necessary, using the same rules as Bazel's ShellEscaper
// so that the resulting bytes are identical to the parameter files
// that Bazel writes. A string is left unmodified iff it only consists
// of ASCII letters, digits and the characters "@%-_+:,./", with tilde
// ('~') additionally being permitted at any position other than the
// first. The empty string yields a pair of single quotes, and all
// other strings are enclosed in single quotes, with every single quote
// contained in them being replaced by a quote-terminating, escaped and
// quote-reopening sequence.
func bazelShellEscape(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			strings.IndexByte("@%-_+:,./", c) >= 0,
			c == '~' && i > 0:
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
