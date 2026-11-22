package analysis

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"bonanza.build/pkg/label"
	model_command "bonanza.build/pkg/model/command"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/btree"
	"bonanza.build/pkg/model/core/inlinedtree"
	"bonanza.build/pkg/model/evaluation"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_format "bonanza.build/pkg/model/format"
	model_parser "bonanza.build/pkg/model/parser"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_command_pb "bonanza.build/pkg/proto/model/command"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/starlark/eval"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

var runEnvironmentInfoProviderIdentifier = util.Must(label.NewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%RunEnvironmentInfo"))

// encodeTest extracts the properties that are needed to execute a
// test from the provider instances returned by a test rule.
func (tc *targetContext[TReference, TMetadata]) encodeTest(
	thread *eval.Thread,
	providerInstances []*model_starlark.Struct[TReference, TMetadata],
	test *model_starlark_pb.RuleTarget_Test,
	patcher *model_core.ReferenceMessagePatcher[TMetadata],
) (*model_analysis_pb.ConfiguredTarget_Value_Test, error) {
	encodedTest := &model_analysis_pb.ConfiguredTarget_Value_Test{
		Size:    test.Size,
		Timeout: test.Timeout,
	}
	for _, providerInstance := range providerInstances {
		providerIdentifier, err := providerInstance.GetProviderIdentifier()
		if err != nil {
			return nil, err
		}
		switch providerIdentifier {
		case defaultInfoProviderIdentifier:
			filesToRun, err := providerInstance.Attr(thread, "files_to_run")
			if err != nil {
				return nil, fmt.Errorf("failed to obtain files to run of test: %w", err)
			}
			filesToRunStruct, ok := filesToRun.(*model_starlark.Struct[TReference, TMetadata])
			if !ok {
				return nil, errors.New("files_to_run of DefaultInfo is not a struct")
			}
			encodedFilesToRun, executable, err := tc.newFilesToRunProviderFromStruct(thread, filesToRunStruct)
			if err != nil {
				return nil, fmt.Errorf("failed to encode files to run of test: %w", err)
			}
			if executable == nil {
				encodedFilesToRun.Discard()
				return nil, errors.New("test rule did not provide an executable through DefaultInfo")
			}
			encodedTest.FilesToRun = encodedFilesToRun.Merge(patcher)
		case runEnvironmentInfoProviderIdentifier:
			environment, err := providerInstance.Attr(thread, "environment")
			if err != nil {
				return nil, fmt.Errorf("failed to obtain environment of RunEnvironmentInfo: %w", err)
			}
			environmentDict, ok := environment.(eval.Dict)
			if !ok {
				return nil, errors.New("environment of RunEnvironmentInfo is not a dict")
			}
			variables := map[string]string{}
			var errIter error
			for name, value := range environmentDict.Items(thread, &errIter) {
				nameStr, ok1, err1 := eval.AsString(thread, name)
				valueStr, ok2, err2 := eval.AsString(thread, value)
				if err := errors.Join(err1, err2); err != nil {
					return nil, err
				}
				if !ok1 || !ok2 {
					return nil, errors.New("environment of RunEnvironmentInfo must map strings to strings")
				}
				variables[nameStr] = valueStr
			}
			if errIter != nil {
				return nil, errIter
			}
			for _, name := range slices.Sorted(maps.Keys(variables)) {
				encodedTest.Environment = append(encodedTest.Environment, &model_analysis_pb.ConfiguredTarget_Value_Test_EnvironmentVariable{
					Name:  name,
					Value: variables[name],
				})
			}
		}
	}
	if encodedTest.FilesToRun == nil {
		return nil, errors.New("test rule did not provide DefaultInfo")
	}

	// Like Bazel, execute tests on the platform of the "test"
	// execution group if the rule declares one.
	execGroups := tc.namedExecGroups.Message
	execGroupIndex, ok := slices.BinarySearchFunc(execGroups, "test", func(execGroup *model_starlark_pb.NamedExecGroup, name string) int {
		return strings.Compare(execGroup.Name, name)
	})
	if !ok {
		execGroupIndex, ok = slices.BinarySearchFunc(execGroups, "", func(execGroup *model_starlark_pb.NamedExecGroup, name string) int {
			return strings.Compare(execGroup.Name, name)
		})
		if !ok {
			return nil, errors.New("test rule does not have a default execution group")
		}
	}
	encodedTest.PlatformPkixPublicKey = tc.execGroups[execGroupIndex].platformPkixPublicKey
	return encodedTest, nil
}

// getTestTimeoutSeconds returns the number of seconds a test may run,
// using the same values as Bazel's default for --test_timeout.
func getTestTimeoutSeconds(timeout model_starlark_pb.TestTimeout) int64 {
	switch timeout {
	case model_starlark_pb.TestTimeout_SHORT:
		return 60
	case model_starlark_pb.TestTimeout_MODERATE:
		return 300
	case model_starlark_pb.TestTimeout_LONG:
		return 900
	default:
		return 3600
	}
}

// testRunnerScript is a shell script that sets up the environment in
// which a test is executed, similar to Bazel's test-setup.sh, and
// subsequently executes the test. The input root relative path of the
// test's executable is provided as the first argument. Standard error
// is redirected to standard output, so that a single log is captured.
const testRunnerScript = `set -e
root=$PWD
executable=$1
shift
export RUNFILES_DIR="$root/$executable.runfiles"
export TEST_SRCDIR="$RUNFILES_DIR"
export JAVA_RUNFILES="$RUNFILES_DIR"
export PYTHON_RUNFILES="$RUNFILES_DIR"
export TEST_BINARY="$executable"
export TEST_TMPDIR="$root/_bonanza_test/tmp"
export TEST_UNDECLARED_OUTPUTS_DIR="$root/_bonanza_test/outputs"
export XML_OUTPUT_FILE="$root/_bonanza_test/test.xml"
export HOME="$TEST_TMPDIR"
mkdir -p "$TEST_TMPDIR" "$TEST_UNDECLARED_OUTPUTS_DIR" "$RUNFILES_DIR/$TEST_WORKSPACE"
cd "$RUNFILES_DIR/$TEST_WORKSPACE"
exec "$root/$executable" "$@" 2>&1
`

// maximumTestLogSizeBytes is the maximum number of bytes of a test's
// log that is returned as part of its result.
const maximumTestLogSizeBytes = 64 * 1024

func (c *baseComputer[TReference, TMetadata]) ComputeTestResultValue(ctx context.Context, key model_core.Message[*model_analysis_pb.TestResult_Key, TReference], e TestResultEnvironment[TReference, TMetadata]) (PatchedTestResultValue[TMetadata], error) {
	// Like TargetProviders, analyze the test in the configuration
	// yielded by its incoming edge transition.
	incomingEdgeTransition := e.GetTargetIncomingEdgeTransitionValue(
		model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.TargetIncomingEdgeTransition_Key {
			return &model_analysis_pb.TargetIncomingEdgeTransition_Key{
				Label:                  key.Message.Label,
				ConfigurationReference: model_core.Patch(e, model_core.Nested(key, key.Message.ConfigurationReference)).Merge(patcher),
			}
		}),
	)
	if !incomingEdgeTransition.IsSet() {
		return PatchedTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	configurationReference := model_core.Nested(incomingEdgeTransition, incomingEdgeTransition.Message.OutputConfigurationReference)
	configuredTarget := e.GetConfiguredTargetValue(
		model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.ConfiguredTarget_Key {
			return &model_analysis_pb.ConfiguredTarget_Key{
				Label:   key.Message.Label,
				Variant: newTargetVariant(model_core.Patch(e, configurationReference).Merge(patcher), nil),
			}
		}),
	)
	actionEncoders, gotActionEncoders := e.GetActionEncodersObject(&model_analysis_pb.ActionEncoders_Key{})
	directoryCreationParameters, gotDirectoryCreationParameters := e.GetDirectoryCreationParametersObject(&model_analysis_pb.DirectoryCreationParameters_Key{})
	directoryCreationParametersMessage := e.GetDirectoryCreationParametersValue(&model_analysis_pb.DirectoryCreationParameters_Key{})
	directoryReaders, gotDirectoryReaders := e.GetDirectoryAccessParametersObject(&model_analysis_pb.DirectoryAccessParameters_Key{})
	fileCreationParametersMessage := e.GetFileCreationParametersValue(&model_analysis_pb.FileCreationParameters_Key{})
	fileReader, gotFileReader := e.GetFileAccessParametersObject(&model_analysis_pb.FileAccessParameters_Key{})
	if !configuredTarget.IsSet() ||
		!gotActionEncoders ||
		!gotDirectoryCreationParameters ||
		!directoryCreationParametersMessage.IsSet() ||
		!gotDirectoryReaders ||
		!fileCreationParametersMessage.IsSet() ||
		!gotFileReader {
		return PatchedTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	test := configuredTarget.Message.Test
	if test == nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("target %#v is not a test", key.Message.Label)
	}

	// Create an input root containing the test's executable and
	// its runfiles.
	var rootDirectory model_filesystem.MergedDirectory[TReference]
	executablePath, err := addFilesToRunToMergedDirectory(e, model_core.Nested(configuredTarget, test.FilesToRun), &rootDirectory)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to add test executable to input root: %w", err)
	}
	createdRootDirectory, err := model_filesystem.CreateMergedDirectory(
		ctx,
		directoryReaders.DirectoryEntries,
		directoryCreationParameters,
		e,
		model_filesystem.MergeConflictPolicyError,
		&rootDirectory,
	)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, err
	}
	inputRootReference, err := createdRootDirectory.CaptureDirectoryReference(ctx, directoryCreationParameters, e)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, err
	}

	// Create the command, setting the environment variables that
	// Bazel sets for tests.
	timeoutSeconds := getTestTimeoutSeconds(test.Timeout)
	environment := map[string]string{
		"TEST_SIZE":      strings.ToLower(test.Size.String()),
		"TEST_TARGET":    key.Message.Label,
		"TEST_TIMEOUT":   strconv.FormatInt(timeoutSeconds, 10),
		"TEST_WORKSPACE": componentMainWorkspaceName.String(),
		"TZ":             "UTC",
	}
	for _, variable := range test.Environment {
		environment[variable.Name] = variable.Value
	}
	referenceFormat := c.referenceFormat
	actionEncoder := actionEncoders.Encoder
	environmentVariableList, envParentNodeComputer, err := convertDictToEnvironmentVariableList(ctx, environment, actionEncoder, referenceFormat, e)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, err
	}
	argumentsBuilder, argumentsParentNodeComputer := newArgumentsBuilder(ctx, actionEncoder, referenceFormat, e)
	for _, argument := range []string{"/bin/sh", "-c", testRunnerScript, "bonanza-test-runner", executablePath} {
		if err := argumentsBuilder.PushChild(
			model_core.NewSimplePatchedMessage[TMetadata](&model_command_pb.ArgumentList_Element{
				Level: &model_command_pb.ArgumentList_Element_Leaf{
					Leaf: argument,
				},
			}),
		); err != nil {
			return PatchedTestResultValue[TMetadata]{}, err
		}
	}
	argumentList, err := argumentsBuilder.FinalizeList()
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, err
	}

	command, err := inlinedtree.Build(
		inlinedtree.CandidateList[*model_command_pb.Command, TMetadata]{
			inlinedtree.AlwaysInline(
				model_core.NewReferenceMessagePatcher[TMetadata](),
				func(command model_core.PatchedMessage[*model_command_pb.Command, TMetadata]) {
					command.Message.DirectoryCreationParameters = directoryCreationParametersMessage.Message.DirectoryCreationParameters
					command.Message.FileCreationParameters = fileCreationParametersMessage.Message.FileCreationParameters
					command.Message.WorkingDirectory = (*path.Trace)(nil).GetUNIXString()
				},
			),
			{
				ExternalMessage: argumentList.ToMarshaler(model_format.NewProtoList[model_command_pb.ArgumentList_Element]()),
				Encoder:         actionEncoder,
				ParentAppender: func(
					command model_core.PatchedMessage[*model_command_pb.Command, TMetadata],
					externalObject *model_core.Decodable[model_core.CreatedObject[TMetadata]],
				) error {
					arguments, err := btree.MaybeMergeNodes(argumentList.Message, externalObject, command.Patcher, argumentsParentNodeComputer)
					if err != nil {
						return err
					}
					command.Message.Arguments = arguments
					return nil
				},
			},
			{
				ExternalMessage: environmentVariableList.ToMarshaler(model_format.NewProtoList[model_command_pb.EnvironmentVariableList_Element]()),
				Encoder:         actionEncoder,
				ParentAppender: func(
					command model_core.PatchedMessage[*model_command_pb.Command, TMetadata],
					externalObject *model_core.Decodable[model_core.CreatedObject[TMetadata]],
				) error {
					environmentVariables, err := btree.MaybeMergeNodes(environmentVariableList.Message, externalObject, command.Patcher, envParentNodeComputer)
					if err != nil {
						return err
					}
					command.Message.EnvironmentVariables = environmentVariables
					return nil
				},
			},
		},
		inlinedtree.Options{
			ReferenceFormat:  referenceFormat,
			MaximumSizeBytes: 32 * 1024,
		},
	)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, err
	}
	createdCommand, err := model_core.MarshalAndEncodeDeterministic(
		command.ToMarshaler(model_format.NewProto[model_command_pb.Command]()),
		referenceFormat,
		actionEncoder,
	)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create command: %w", err)
	}

	action, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) (encoding.BinaryMarshaler, error) {
		commandReference, err := patcher.CaptureAndAddDecodableReference(ctx, createdCommand, e)
		if err != nil {
			return nil, err
		}
		return model_format.NewProto[model_command_pb.Action]().NewMarshaler(&model_command_pb.Action{
			CommandReference:   commandReference,
			InputRootReference: inputRootReference.Merge(patcher),
		}), nil
	})
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create action: %w", err)
	}
	createdAction, err := model_core.MarshalAndEncodeDeterministic(action, referenceFormat, actionEncoder)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to encode action: %w", err)
	}

	// Execute the test. Request the raw action result, so that
	// tests that time out can be reported as such, as opposed to
	// causing evaluation to fail.
	rawActionResultKey, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) (*model_analysis_pb.RawActionResult_Key, error) {
		actionReference, err := patcher.CaptureAndAddDecodableReference(ctx, createdAction, e)
		if err != nil {
			return nil, err
		}
		return &model_analysis_pb.RawActionResult_Key{
			ExecuteRequest: &model_analysis_pb.ExecuteRequest{
				PlatformPkixPublicKey: test.PlatformPkixPublicKey,
				ActionReference:       actionReference,
				ExecutionTimeout:      &durationpb.Duration{Seconds: timeoutSeconds},
			},
		}, nil
	})
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create action result key: %w", err)
	}
	rawActionResult := e.GetRawActionResultValue(rawActionResultKey)
	if !rawActionResult.IsSet() {
		return PatchedTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	result, err := model_parser.Dereference(ctx, actionEncoders.Readers.CommandResult, model_core.Nested(rawActionResult, rawActionResult.Message.ResultReference))
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to read result of test: %w", err)
	}

	testResult := &model_analysis_pb.TestResult_Value{
		ExitCode: result.Message.ExitCode,
	}
	if err := status.ErrorProto(result.Message.Status); err != nil {
		if status.Code(err) != codes.DeadlineExceeded {
			return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to execute test: %w", err)
		}
		testResult.Status = model_analysis_pb.TestResult_Value_TIMEOUT
	} else if result.Message.ExitCode == 0 {
		testResult.Status = model_analysis_pb.TestResult_Value_PASSED
	} else {
		testResult.Status = model_analysis_pb.TestResult_Value_FAILED
	}

	// Return the tail of the test's log.
	outputs, err := model_parser.MaybeDereference(ctx, directoryReaders.CommandOutputs, model_core.Nested(result, result.Message.OutputsReference))
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to obtain outputs of test: %w", err)
	}
	log, err := model_command.MaybeReadLogByName(ctx, fileReader, outputs, model_command.StdoutLogName, 1<<30)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to read log of test: %w", err)
	}
	testResult.Log, testResult.LogTruncated = getLogTail(log, maximumTestLogSizeBytes)
	return model_core.NewSimplePatchedMessage[TMetadata](testResult), nil
}

// getLogTail returns the last bytes of a log, ensuring that the result
// is valid UTF-8.
func getLogTail(log []byte, maximumSizeBytes int) (string, bool) {
	truncated := false
	if len(log) > maximumSizeBytes {
		log = log[len(log)-maximumSizeBytes:]
		truncated = true
		// Don't start in the middle of a UTF-8 sequence.
		for len(log) > 0 && !utf8.RuneStart(log[0]) {
			log = log[1:]
		}
	}
	return strings.ToValidUTF8(string(log), "�"), truncated
}
