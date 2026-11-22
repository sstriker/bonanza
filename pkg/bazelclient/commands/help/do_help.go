package help

import (
	"fmt"
	"io"
	"os"
	"strings"

	"bonanza.build/pkg/bazelclient/arguments"
)

// DoHelp implements the "bazel help" command. Without arguments, it
// lists the commands that are supported. If the name of a command is
// provided, it lists the flags that are accepted by that command.
func DoHelp(args *arguments.HelpCommand) {
	if err := writeHelp(os.Stdout, args.Arguments, args.HelpFlags.HelpVerbosity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func writeHelp(w io.Writer, helpArguments []string, verbosity arguments.HelpVerbosity) error {
	switch len(helpArguments) {
	case 0:
		fmt.Fprintln(w, "Usage: bazel <command> <options> ...")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Available commands:")
		for _, commandName := range arguments.CommandNames {
			fmt.Fprintf(w, "  %s\n", commandName)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Getting more help:")
		fmt.Fprintln(w, "  bazel help <command>")
		fmt.Fprintln(w, "                   Prints help and options for <command>.")
		return nil
	case 1:
		commandName := helpArguments[0]
		flags, ok := arguments.GetCommandFlags(commandName)
		if !ok {
			return fmt.Errorf("ERROR: %#v is not a known command", commandName)
		}
		fmt.Fprintf(w, "Usage: bazel %s <options> ...\n", commandName)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Options for %s:\n", commandName)
		for _, flag := range flags {
			name := "--" + flag.LongName
			if flag.ShortName != "" {
				name += " [-" + flag.ShortName + "]"
			}
			fmt.Fprintf(w, "  %s\n", name)
			if verbosity != arguments.HelpVerbosity_Short && flag.Description != "" {
				description := flag.Description
				if verbosity != arguments.HelpVerbosity_Long {
					// Only show the first sentence.
					if i := strings.Index(description, ". "); i >= 0 {
						description = description[:i+1]
					}
				}
				fmt.Fprintf(w, "    %s\n", description)
			}
		}
		return nil
	default:
		return fmt.Errorf("ERROR: you must specify exactly one command")
	}
}
