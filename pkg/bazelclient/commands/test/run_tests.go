package test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/bazelclient/commands"
	"bonanza.build/pkg/bazelclient/commands/build"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	model_core "bonanza.build/pkg/model/core"
	model_format "bonanza.build/pkg/model/format"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
)

const (
	// Exit codes used by Bazel.
	exitCodeTestsFailed  = 3
	exitCodeNoTestsFound = 4
)

// DoTest implements the "bazel test" command, which builds the targets
// matching the provided target patterns, and executes the tests among
// them remotely.
func DoTest(args *arguments.TestCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)
	commands.ValidateInsideWorkspace(logger, "test", workspacePath)

	evaluation := build.EvaluateBuildResult(logger, "test", &args.CommonFlags, &args.BuildFlags, args.Arguments, args.BuildSettingOverrides, workspacePath, true)
	if outcomesLink, ok := evaluation.GetOutcomesLink(); ok {
		logger.Info(formatted.Join(formatted.Text("Got build results "), outcomesLink))
	}

	opaqueValue, err := evaluation.GetRequestedValue(context.Background())
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to obtain test results: %s", err))
	}
	buildResult, err := model_core.UnmarshalOpaque(opaqueValue, model_format.NewProto[model_analysis_pb.BuildResult_Value]())
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to unmarshal test results: %s", err))
	}

	if exitCode := writeTestSummary(os.Stdout, buildResult.Message.Tests, args.TestFlags.TestOutput); exitCode != 0 {
		os.Exit(exitCode)
	}
}

// writeTestSummary writes the results of the tests that were executed
// in a format similar to Bazel's, and returns the exit code with which
// the command should terminate.
func writeTestSummary(w io.Writer, tests []*model_analysis_pb.BuildResult_Value_Test, testOutput arguments.TestOutput) int {
	if len(tests) == 0 {
		fmt.Fprintln(w, "ERROR: No test targets were found, yet testing was requested")
		return exitCodeNoTestsFound
	}

	// Print logs of tests, depending on --test_output.
	passed := 0
	for _, test := range tests {
		result := test.Result
		if result.GetStatus() == model_analysis_pb.TestResult_Value_PASSED {
			passed++
		}
		if testOutput == arguments.TestOutput_All ||
			testOutput == arguments.TestOutput_Streamed ||
			(testOutput == arguments.TestOutput_Errors && result.GetStatus() != model_analysis_pb.TestResult_Value_PASSED) {
			fmt.Fprintf(w, "==================== Test output for %s:\n", test.Label)
			if result.GetLogTruncated() {
				fmt.Fprintln(w, "(log truncated)")
			}
			fmt.Fprint(w, result.GetLog())
			if log := result.GetLog(); log != "" && !strings.HasSuffix(log, "\n") {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, strings.Repeat("=", 80))
		}
	}

	// Print the status of each test.
	for _, test := range tests {
		var status string
		switch test.Result.GetStatus() {
		case model_analysis_pb.TestResult_Value_PASSED:
			status = "PASSED"
		case model_analysis_pb.TestResult_Value_FAILED:
			status = fmt.Sprintf("FAILED (exit code %d)", test.Result.GetExitCode())
		case model_analysis_pb.TestResult_Value_TIMEOUT:
			status = "TIMEOUT"
		default:
			status = "NO STATUS"
		}
		fmt.Fprintf(w, "%-70s %s\n", test.Label, status)
	}

	failed := len(tests) - passed
	fmt.Fprintln(w)
	summary := fmt.Sprintf("Executed %d out of %d %s: %d %s", len(tests), len(tests), pluralize(len(tests), "test", "tests"), passed, pluralize(passed, "test passes", "tests pass"))
	if failed > 0 {
		summary += fmt.Sprintf(" and %d %s", failed, pluralize(failed, "fails", "fail"))
	}
	fmt.Fprintln(w, summary+".")
	if failed > 0 {
		return exitCodeTestsFailed
	}
	return 0
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
