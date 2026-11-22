package test

import (
	"bytes"
	"testing"

	"bonanza.build/pkg/bazelclient/arguments"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/stretchr/testify/require"
)

func TestWriteTestSummary(t *testing.T) {
	passedTest := &model_analysis_pb.BuildResult_Value_Test{
		Label: "@@main+//foo:passing_test",
		Result: &model_analysis_pb.TestResult_Value{
			Status: model_analysis_pb.TestResult_Value_PASSED,
			Log:    "all good\n",
		},
	}
	failedTest := &model_analysis_pb.BuildResult_Value_Test{
		Label: "@@main+//foo:failing_test",
		Result: &model_analysis_pb.TestResult_Value{
			Status:   model_analysis_pb.TestResult_Value_FAILED,
			ExitCode: 1,
			Log:      "assertion failed",
		},
	}

	t.Run("NoTests", func(t *testing.T) {
		var b bytes.Buffer
		require.Equal(t, 4, writeTestSummary(&b, nil, arguments.TestOutput_Summary))
		require.Equal(t, "ERROR: No test targets were found, yet testing was requested\n", b.String())
	})

	t.Run("AllPassed", func(t *testing.T) {
		var b bytes.Buffer
		require.Equal(t, 0, writeTestSummary(&b, []*model_analysis_pb.BuildResult_Value_Test{passedTest}, arguments.TestOutput_Summary))
		require.Contains(t, b.String(), "@@main+//foo:passing_test")
		require.Contains(t, b.String(), " PASSED\n")
		require.Contains(t, b.String(), "Executed 1 out of 1 test: 1 test passes.\n")
		require.NotContains(t, b.String(), "all good")
	})

	t.Run("FailureWithErrorsOutput", func(t *testing.T) {
		// Only the logs of failed tests are printed.
		var b bytes.Buffer
		require.Equal(t, 3, writeTestSummary(&b, []*model_analysis_pb.BuildResult_Value_Test{failedTest, passedTest}, arguments.TestOutput_Errors))
		require.Contains(t, b.String(), "==================== Test output for @@main+//foo:failing_test:\nassertion failed\n")
		require.NotContains(t, b.String(), "all good")
		require.Contains(t, b.String(), " FAILED (exit code 1)\n")
		require.Contains(t, b.String(), "Executed 2 out of 2 tests: 1 test passes and 1 fails.\n")
	})

	t.Run("AllOutput", func(t *testing.T) {
		var b bytes.Buffer
		require.Equal(t, 3, writeTestSummary(&b, []*model_analysis_pb.BuildResult_Value_Test{failedTest, passedTest}, arguments.TestOutput_All))
		require.Contains(t, b.String(), "all good\n")
		require.Contains(t, b.String(), "assertion failed\n")
	})
}
