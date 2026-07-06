package analysis

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"

	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/protobuf/proto"

	"go.starlark.net/starlark"
)

var (
	analysisFailureProviderIdentifier     = util.Must(label.NewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%AnalysisFailure"))
	analysisFailureInfoProviderIdentifier = util.Must(label.NewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%AnalysisFailureInfo"))
)

// allowAnalysisFailuresBuildSettingLabelStr is the label of the build
// setting backing Bazel's --allow_analysis_failures command line
// option, which analysis_test_transition() sets to True when an
// analysis test declares expect_failure = True.
const allowAnalysisFailuresBuildSettingLabelStr = "@@bazel_tools+//command_line_option:allow_analysis_failures"

// computeAnalysisFailureConfiguredTargetValue creates a configured
// target value containing an AnalysisFailureInfo provider and an empty
// DefaultInfo provider. It is emitted in place of a regular configured
// target value if analysis of a target fails while
// --allow_analysis_failures is enabled, so that analysis test rules can
// assert on the failure.
//
// The resulting AnalysisFailureInfo's causes contain an AnalysisFailure
// describing the target's own failure (if failureMessage is non-empty),
// followed by the causes of any direct dependencies whose analysis
// failed.
func (c *baseComputer[TReference, TMetadata]) computeAnalysisFailureConfiguredTargetValue(
	ctx context.Context,
	e ConfiguredTargetEnvironment[TReference, TMetadata],
	key model_core.Message[*model_analysis_pb.ConfiguredTarget_Key, TReference],
	targetLabel label.CanonicalLabel,
	emptyDefaultInfo model_core.Message[*model_starlark_pb.Struct, TReference],
	failureMessage string,
	failedDepProviders []model_core.Message[*model_starlark_pb.Struct, TReference],
) (PatchedConfiguredTargetValue[TMetadata], error) {
	allBuiltinsModulesNames := e.GetBuiltinsModuleNamesValue(&model_analysis_pb.BuiltinsModuleNames_Key{})
	analysisFailureProviderValue := e.GetCompiledBzlFileGlobalValue(&model_analysis_pb.CompiledBzlFileGlobal_Key{
		Identifier: analysisFailureProviderIdentifier.String(),
	})
	analysisFailureInfoProviderValue := e.GetCompiledBzlFileGlobalValue(&model_analysis_pb.CompiledBzlFileGlobal_Key{
		Identifier: analysisFailureInfoProviderIdentifier.String(),
	})
	if !allBuiltinsModulesNames.IsSet() ||
		!analysisFailureProviderValue.IsSet() ||
		!analysisFailureInfoProviderValue.IsSet() {
		return PatchedConfiguredTargetValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	decodeProvider := func(providerValue model_core.Message[*model_analysis_pb.CompiledBzlFileGlobal_Value, TReference], identifier label.CanonicalStarlarkIdentifier) (*model_starlark.Provider[TReference, TMetadata], error) {
		kind, ok := providerValue.Message.Global.GetKind().(*model_starlark_pb.Value_Provider)
		if !ok {
			return nil, fmt.Errorf("%#v is not a provider", identifier.String())
		}
		provider, err := model_starlark.DecodeProvider[TReference, TMetadata](model_core.Nested(providerValue, kind.Provider))
		if err != nil {
			return nil, fmt.Errorf("failed to decode provider %#v: %w", identifier.String(), err)
		}
		return provider, nil
	}
	analysisFailureProvider, err := decodeProvider(analysisFailureProviderValue, analysisFailureProviderIdentifier)
	if err != nil {
		return PatchedConfiguredTargetValue[TMetadata]{}, err
	}
	analysisFailureInfoProvider, err := decodeProvider(analysisFailureInfoProviderValue, analysisFailureInfoProviderIdentifier)
	if err != nil {
		return PatchedConfiguredTargetValue[TMetadata]{}, err
	}

	thread := c.newStarlarkThread(ctx, e, allBuiltinsModulesNames.Message.BuiltinsModuleNames)
	identifierGenerator, err := c.getReferenceEqualIdentifierGenerator(model_core.Nested(key, proto.Message(key.Message)))
	if err != nil {
		return PatchedConfiguredTargetValue[TMetadata]{}, fmt.Errorf("failed to obtain identifier generator for reference equal values: %w", err)
	}
	thread.SetLocal(model_starlark.ReferenceEqualIdentifierGeneratorKey, identifierGenerator)

	var causes []any
	if failureMessage != "" {
		cause, err := analysisFailureProvider.Instantiate(thread, nil, []starlark.Tuple{
			{starlark.String("label"), model_starlark.NewLabel[TReference, TMetadata](targetLabel.AsResolved())},
			{starlark.String("message"), starlark.String(failureMessage)},
		})
		if err != nil {
			return PatchedConfiguredTargetValue[TMetadata]{}, fmt.Errorf("failed to instantiate AnalysisFailure: %w", err)
		}
		causes = append(causes, cause)
	}
	for _, failedDepProvider := range failedDepProviders {
		depCauses, err := model_starlark.GetStructFieldValue(
			ctx,
			c.valueReaders.List,
			model_core.Nested(failedDepProvider, failedDepProvider.Message.Fields),
			"causes",
		)
		if err != nil {
			return PatchedConfiguredTargetValue[TMetadata]{}, fmt.Errorf("failed to obtain field \"causes\" of AnalysisFailureInfo provider of a dependency: %w", err)
		}
		depCausesDepset, ok := depCauses.Message.Kind.(*model_starlark_pb.Value_Depset)
		if !ok {
			return PatchedConfiguredTargetValue[TMetadata]{}, errors.New("field \"causes\" of AnalysisFailureInfo provider of a dependency is not a depset")
		}
		for _, element := range depCausesDepset.Depset.Elements {
			causes = append(causes, model_core.Nested(depCauses, element))
		}
	}

	analysisFailureInfo, err := analysisFailureInfoProvider.Instantiate(thread, nil, []starlark.Tuple{
		{
			starlark.String("causes"),
			model_starlark.NewDepset(
				model_starlark.NewDepsetContentsFromList[TReference, TMetadata](causes, model_starlark_pb.Depset_DEFAULT),
				identifierGenerator,
			),
		},
	})
	if err != nil {
		return PatchedConfiguredTargetValue[TMetadata]{}, fmt.Errorf("failed to instantiate AnalysisFailureInfo: %w", err)
	}

	return model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) (*model_analysis_pb.ConfiguredTarget_Value, error) {
		encodedAnalysisFailureInfo, _, err := analysisFailureInfo.Encode(map[starlark.Value]struct{}{}, c.getValueEncodingOptions(ctx, e, nil))
		if err != nil {
			return nil, fmt.Errorf("failed to encode AnalysisFailureInfo provider instance: %w", err)
		}
		providerInstances := []*model_starlark_pb.Struct{
			encodedAnalysisFailureInfo.Merge(patcher),
			model_core.Patch(e, emptyDefaultInfo).Merge(patcher),
		}
		slices.SortFunc(providerInstances, func(a, b *model_starlark_pb.Struct) int {
			return strings.Compare(
				a.ProviderInstanceProperties.ProviderIdentifier,
				b.ProviderInstanceProperties.ProviderIdentifier,
			)
		})
		return &model_analysis_pb.ConfiguredTarget_Value{
			ProviderInstances: providerInstances,
		}, nil
	})
}
