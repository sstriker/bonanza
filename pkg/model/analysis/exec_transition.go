package analysis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/btree"
	"bonanza.build/pkg/model/evaluation"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/starlark/eval"

	"github.com/buildbarn/bb-storage/pkg/util"
)

type createInitialConfigurationEnvironment[TReference any, TMetadata model_core.ReferenceMetadata] interface {
	valueEncodingEnvironment[TReference, TMetadata]
	labelResolverEnvironment[TReference]
	getExpectedTransitionOutputEnvironment[TReference, TMetadata]
}

// initialConfigurationBuildSetting contains all of the values that were
// provided for a single build setting. Depending on whether the build
// setting type is repeatable, either the last or all values are used to
// determine the build setting's definitive value.
type initialConfigurationBuildSetting[TReference any] struct {
	expectedOutput expectedTransitionOutput[TReference]
	values         []string
}

func (c *baseComputer[TReference, TMetadata]) createInitialConfiguration(
	ctx context.Context,
	e createInitialConfigurationEnvironment[TReference, TMetadata],
	analysisEncoders *AnalysisEncoders[TReference],
	starlarkValueEncoders *StarlarkValueEncoders[TReference],
	thread *eval.Thread,
	rootPackage label.CanonicalPackage,
	configuration *model_analysis_pb.BuildResult_Key_Configuration,
) (model_core.PatchedMessage[*model_core_pb.DecodableReference, TMetadata], error) {
	valueEncodingOptionsFactory, err := c.newValueEncodingOptionsFactory(e, starlarkValueEncoders)
	if err != nil {
		return model_core.PatchedMessage[*model_core_pb.DecodableReference, TMetadata]{}, err
	}

	// Walk over all provided build setting overrides and group all
	// values belonging to the same build setting together.
	var buildSettings []initialConfigurationBuildSetting[TReference]
	originalBuildSettingLabelIndices := map[string]int{}
	visibleBuildSettingLabelIndices := map[string]int{}
	missingDependencies := false
	for _, buildSettingOverride := range configuration.BuildSettingOverrides {
		index, ok := originalBuildSettingLabelIndices[buildSettingOverride.Label]
		if !ok {
			apparentBuildSettingLabel, err := rootPackage.AppendLabel(buildSettingOverride.Label)
			if err != nil {
				return model_core.PatchedMessage[*model_core_pb.DecodableReference, TMetadata]{}, fmt.Errorf("invalid build setting label %#v: %w", buildSettingOverride.Label, err)
			}
			expectedOutput, err := getExpectedTransitionOutput[TReference, TMetadata](e, rootPackage, apparentBuildSettingLabel)
			if err != nil {
				if errors.Is(err, evaluation.ErrMissingDependency) {
					missingDependencies = true
					continue
				}
			}
			if !expectedOutput.isFlag {
				return model_core.PatchedMessage[*model_core_pb.DecodableReference, TMetadata]{}, fmt.Errorf("build setting with label %#v is not callable on the command line", expectedOutput.label)
			}
			index, ok = visibleBuildSettingLabelIndices[expectedOutput.label]
			if !ok {
				index = len(buildSettings)
				buildSettings = append(buildSettings, initialConfigurationBuildSetting[TReference]{
					expectedOutput: expectedOutput,
				})
				visibleBuildSettingLabelIndices[expectedOutput.label] = index
			}
			originalBuildSettingLabelIndices[buildSettingOverride.Label] = index
		}
		buildSettings[index].values = append(buildSettings[index].values, buildSettingOverride.Value)
	}
	if missingDependencies {
		return model_core.PatchedMessage[*model_core_pb.DecodableReference, TMetadata]{}, evaluation.ErrMissingDependency
	}

	// Sort all build settings by label, as that's the order in
	// which applyTransition expects them to be to insert them into
	// the resulting configuration.
	slices.SortFunc(buildSettings, func(a, b initialConfigurationBuildSetting[TReference]) int {
		return strings.Compare(a.expectedOutput.label, b.expectedOutput.label)
	})

	labelResolver := newLabelResolver(e)
	buildSettingValuesToApply := make([]buildSettingValueToApply[TReference], 0, len(buildSettings))
	for _, buildSetting := range buildSettings {
		expectedOutput := &buildSetting.expectedOutput
		canonicalizedValue, err := expectedOutput.canonicalizer.CanonicalizeStringList(buildSetting.values, labelResolver)
		if err != nil {
			return model_core.PatchedMessage[*model_core_pb.DecodableReference, TMetadata]{}, fmt.Errorf("invalid values for build setting label %#v: %w", expectedOutput.label, err)
		}
		buildSettingValuesToApply = append(buildSettingValuesToApply, buildSettingValueToApply[TReference]{
			label:              expectedOutput.label,
			canonicalizedValue: canonicalizedValue,
			defaultValue:       expectedOutput.defaultValue,
		})
	}

	return c.applyTransition(
		ctx,
		thread,
		e,
		analysisEncoders,
		model_core.NewSimpleMessage[TReference]((*model_core_pb.DecodableReference)(nil)),
		buildSettingValuesToApply,
		/* keepExistingOverride = */ nil,
		valueEncodingOptionsFactory.newValueEncodingOptions(ctx, nil),
	)
}

func (c *baseComputer[TReference, TMetadata]) ComputeExecTransitionValue(ctx context.Context, key model_core.Message[*model_analysis_pb.ExecTransition_Key, TReference], e ExecTransitionEnvironment[TReference, TMetadata]) (PatchedExecTransitionValue[TMetadata], error) {
	analysisEncoders, gotAnalysisEncoders := getAnalysisEncoders(e)
	starlarkValueEncoders, gotStarlarkValueEncoders := getStarlarkValueEncoders(e)
	fileReader, gotFileReader := getFileReader(e)

	valueEncodingOptionsFactory, err := c.newValueEncodingOptionsFactory(e, starlarkValueEncoders)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, err
	}

	allBuiltinsModulesNames := e.GetBuiltinsModuleNamesValue(&model_analysis_pb.BuiltinsModuleNames_Key{})
	rootModuleValue := e.GetRootModuleValue(&model_analysis_pb.RootModule_Key{})
	if !allBuiltinsModulesNames.IsSet() || !rootModuleValue.IsSet() || !gotAnalysisEncoders || !gotStarlarkValueEncoders || !gotFileReader {
		return PatchedExecTransitionValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	analysisReaders := &analysisEncoders.Readers
	valueReaders := &starlarkValueEncoders.Readers

	rootModuleName, err := label.NewModule(rootModuleValue.Message.RootModuleName)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, fmt.Errorf("invalid root module name: %w", err)
	}
	rootPackage := rootModuleName.ToModuleInstance(nil).GetBareCanonicalRepo().GetRootPackage()

	// Determine the new target platform.
	apparentTargetPlatform, err := label.NewApparentLabel(key.Message.PlatformLabel)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, fmt.Errorf("invalid target platform: %w", err)
	}
	canonicalTargetPlatform, err := label.Canonicalize(
		newLabelResolver(e),
		rootPackage.GetCanonicalRepo(),
		apparentTargetPlatform,
	)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, fmt.Errorf("failed to resolve target platform %#v: %w", apparentTargetPlatform.String(), err)
	}

	// Obtain the identifier of the Starlark transition that is used
	// for exec transitions.
	inputConfigurationReference := model_core.Nested(key, key.Message.InputConfigurationReference)
	execConfigCommandLineOption := "@@bazel_tools+//command_line_option:experimental_exec_config"
	execConfigValue, err := c.getBuildSettingValue(
		ctx,
		e,
		analysisReaders,
		valueReaders,
		rootPackage,
		execConfigCommandLineOption,
		inputConfigurationReference,
	)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, err
	}
	execConfigStr, ok, err := getStringValue(ctx, fileReader, execConfigValue)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, fmt.Errorf("value of build setting %s: %w", execConfigCommandLineOption, err)
	}
	if !ok {
		return PatchedExecTransitionValue[TMetadata]{}, fmt.Errorf("value of build setting %s is not a string", execConfigCommandLineOption)
	}

	// Invoke the exec transition.
	thread, err := c.newStarlarkThread(ctx, e, starlarkValueEncoders, allBuiltinsModulesNames.Message.BuiltinsModuleNames)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, err
	}
	expectedOutputs, outputsDict, err := c.performUserDefinedTransition(
		ctx,
		e,
		analysisReaders,
		valueReaders,
		thread,
		model_core.NewSimpleMessage[TReference](
			&model_starlark_pb.Transition_UserDefined{
				Kind: &model_starlark_pb.Transition_UserDefined_Identifier{
					Identifier: execConfigStr,
				},
			},
		),
		inputConfigurationReference,
		stubbedTransitionAttr{},
	)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, err
	}

	if l := len(outputsDict); l > 1 {
		return PatchedExecTransitionValue[TMetadata]{}, fmt.Errorf("exec transition yielded %d configurations, while exactly 1 was expected", l)
	}
	buildSettingValuesToApply, err := getCanonicalTransitionOutputValuesFromDict(thread, expectedOutputs, slices.Collect(maps.Values(outputsDict))[0])
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, err
	}

	// Inject --platforms into build setting values, as the exec
	// transition does not set it for us.
	commandLineOptionPlatformsLabelStr := commandLineOptionPlatformsLabel.String()
	platformsInsertionIndex, hasExistingPlatforms := sort.Find(
		len(buildSettingValuesToApply),
		func(i int) int {
			return strings.Compare(commandLineOptionPlatformsLabelStr, buildSettingValuesToApply[i].label)
		},
	)
	if hasExistingPlatforms {
		return PatchedExecTransitionValue[TMetadata]{}, fmt.Errorf("exec transition is overriding build setting %#v, which is not expected", commandLineOptionPlatformsLabelStr)
	}
	buildSettingValuesToApply = append(
		append(
			buildSettingValuesToApply[:platformsInsertionIndex:platformsInsertionIndex],
			buildSettingValueToApply[TReference]{
				label: commandLineOptionPlatformsLabelStr,
				canonicalizedValue: eval.NewList([]eval.Value{
					model_starlark.NewLabel[TReference, TMetadata](canonicalTargetPlatform.AsResolved()),
				}),
				// TODO: This is not correct, but likely not harmful.
				defaultValue: model_core.NewSimpleMessage[TReference](&model_starlark_pb.Value{}),
			},
		),
		buildSettingValuesToApply[platformsInsertionIndex:]...,
	)

	// Determine which user defined build settings retain their
	// value in the exec configuration.
	propagatingBuildSettings, err := c.getExecPropagatingBuildSettings(
		ctx,
		e,
		analysisReaders,
		valueReaders,
		thread,
		rootPackage,
		inputConfigurationReference,
	)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, err
	}

	// Create new configuration to include any build settings
	// yielded by the exec transition. All other build settings are
	// reset to their default values, except for user defined build
	// settings that propagate into the exec configuration.
	outputConfigurationReference, err := c.applyTransition(
		ctx,
		thread,
		e,
		analysisEncoders,
		inputConfigurationReference,
		buildSettingValuesToApply,
		func(label string) bool {
			_, ok := propagatingBuildSettings[label]
			return ok
		},
		valueEncodingOptionsFactory.newValueEncodingOptions(ctx, nil),
	)
	if err != nil {
		return PatchedExecTransitionValue[TMetadata]{}, err
	}

	return model_core.NewPatchedMessage(
		&model_analysis_pb.ExecTransition_Value{
			OutputConfigurationReference: outputConfigurationReference.Message,
		},
		outputConfigurationReference.Patcher,
	), nil
}

// execStarlarkFlagPropagation determines which user defined build
// settings (i.e., ones not belonging to
// @bazel_tools//command_line_option) retain their value when
// transitioning to an exec configuration. This mirrors Bazel 9's
// FunctionTransitionUtil.getExecPropagatingStarlarkFlags().
type execStarlarkFlagPropagation struct {
	// Value of --incompatible_exclude_starlark_flags_from_exec_config.
	excludeByDefault bool
	// Canonical labels of build settings provided to
	// --experimental_propagate_custom_flag.
	customFlags map[string]struct{}
	// Canonical packages provided to
	// --experimental_propagate_custom_flag in the form of "//pkg/...".
	customFlagPackages []label.CanonicalPackage
}

func (p *execStarlarkFlagPropagation) shouldPropagate(buildSetting label.CanonicalLabel, scope *model_starlark_pb.BuildSettingScope) bool {
	switch scope.GetKind().(type) {
	case *model_starlark_pb.BuildSettingScope_Universal, *model_starlark_pb.BuildSettingScope_Project:
		// Universal and project scoped build settings always
		// propagate.
		return true
	case *model_starlark_pb.BuildSettingScope_Target, *model_starlark_pb.BuildSettingScope_Exec:
		// TODO: Implement "exec:--<label>" scoping. Bazel 9
		// does not support it.
		return false
	}

	// Build settings that don't declare a scope propagate, unless
	// --incompatible_exclude_starlark_flags_from_exec_config is set.
	// In that case they only propagate if requested through
	// --experimental_propagate_custom_flag.
	if !p.excludeByDefault {
		return true
	}
	if _, ok := p.customFlags[buildSetting.String()]; ok {
		return true
	}
	buildSettingPackage := buildSetting.GetCanonicalPackage().String()
	for _, customFlagPackage := range p.customFlagPackages {
		prefix := customFlagPackage.String()
		if buildSettingPackage == prefix || strings.HasPrefix(buildSettingPackage, strings.TrimSuffix(prefix, "/")+"/") {
			return true
		}
	}
	return false
}

// getExecPropagatingBuildSettings returns the labels of the user defined
// build settings in a configuration whose values need to be retained
// when transitioning to an exec configuration.
func (c *baseComputer[TReference, TMetadata]) getExecPropagatingBuildSettings(
	ctx context.Context,
	e ExecTransitionEnvironment[TReference, TMetadata],
	analysisReaders *AnalysisReaders[TReference],
	valueReaders *model_starlark.ValueReaders[TReference],
	thread *eval.Thread,
	rootPackage label.CanonicalPackage,
	configurationReference model_core.Message[*model_core_pb.DecodableReference, TReference],
) (map[string]struct{}, error) {
	if configurationReference.Message == nil {
		// Default configuration. There is nothing to propagate.
		return nil, nil
	}

	getBuildSettingValue := func(name string) (eval.Value, error) {
		buildSettingLabel := commandLineOptionPlatformsLabel.GetCanonicalPackage().AppendTargetName(util.Must(label.NewTargetName(name))).String()
		encodedValue, err := c.getBuildSettingValue(ctx, e, analysisReaders, valueReaders, rootPackage, buildSettingLabel, configurationReference)
		if err != nil {
			return nil, err
		}
		value, err := model_starlark.DecodeValue[TReference, TMetadata](thread, encodedValue, nil)
		if err != nil {
			return nil, fmt.Errorf("value of build setting %#v: %w", buildSettingLabel, err)
		}
		return value, nil
	}
	excludeByDefault, errExclude := getBuildSettingValue("incompatible_exclude_starlark_flags_from_exec_config")
	customFlags, errCustomFlags := getBuildSettingValue("experimental_propagate_custom_flag")
	if errExclude != nil {
		return nil, errExclude
	}
	if errCustomFlags != nil {
		return nil, errCustomFlags
	}

	excludeByDefaultBool, ok := excludeByDefault.(eval.Bool)
	if !ok {
		return nil, errors.New("value of build setting \"incompatible_exclude_starlark_flags_from_exec_config\" is not a bool")
	}
	p := execStarlarkFlagPropagation{
		excludeByDefault: bool(excludeByDefaultBool),
		customFlags:      map[string]struct{}{},
	}
	missingDependencies := false
	if p.excludeByDefault {
		customFlagsList, err := eval.Elements(thread, customFlags)
		if err != nil {
			return nil, fmt.Errorf("value of build setting \"experimental_propagate_custom_flag\": %w", err)
		}
		labelResolver := newLabelResolver(e)
		for _, customFlag := range customFlagsList {
			customFlagStr, ok := customFlag.(eval.String)
			if !ok {
				return nil, errors.New("value of build setting \"experimental_propagate_custom_flag\" is not a list of strings")
			}
			s := string(customFlagStr)
			if !strings.HasPrefix(s, "//") && !strings.HasPrefix(s, "@") {
				// --define flags are not supported.
				continue
			}
			packagePattern, isPackagePattern := strings.CutSuffix(s, "/...")
			if isPackagePattern {
				s = packagePattern + ":__subpackages__"
			}
			apparentLabel, err := rootPackage.AppendLabel(s)
			if err != nil {
				return nil, fmt.Errorf("invalid value %#v for build setting \"experimental_propagate_custom_flag\": %w", string(customFlagStr), err)
			}
			canonicalLabel, err := label.Canonicalize(labelResolver, rootPackage.GetCanonicalRepo(), apparentLabel)
			if err != nil {
				if errors.Is(err, evaluation.ErrMissingDependency) {
					missingDependencies = true
					continue
				}
				return nil, fmt.Errorf("invalid value %#v for build setting \"experimental_propagate_custom_flag\": %w", string(customFlagStr), err)
			}
			if isPackagePattern {
				p.customFlagPackages = append(p.customFlagPackages, canonicalLabel.GetCanonicalPackage())
			} else {
				p.customFlags[canonicalLabel.String()] = struct{}{}
			}
		}
	}

	// Iterate over all build settings in the configuration and
	// determine whether user defined build settings propagate,
	// based on their scope.
	commandLineOptionPackage := commandLineOptionPlatformsLabel.GetCanonicalPackage()
	propagatingBuildSettings := map[string]struct{}{}
	var errIter error
	for override := range btree.AllLeaves(
		ctx,
		analysisReaders.BuildSettingOverride,
		getBuildSettingOverridesFromReference(configurationReference),
		func(override model_core.Message[*model_analysis_pb.BuildSettingOverride, TReference]) (*model_core_pb.DecodableReference, error) {
			return override.Message.GetParent().GetReference(), nil
		},
		&errIter,
	) {
		leaf := override.Message.GetLeaf()
		if leaf == nil {
			return nil, errors.New("build setting override is not a valid leaf")
		}
		buildSettingLabel, err := label.NewCanonicalLabel(leaf.Label)
		if err != nil {
			return nil, fmt.Errorf("invalid build setting label %#v: %w", leaf.Label, err)
		}
		if buildSettingLabel.GetCanonicalPackage() == commandLineOptionPackage {
			// Native options are set by the exec transition
			// itself.
			continue
		}

		var scope *model_starlark_pb.BuildSettingScope
		targetValue := e.GetTargetValue(&model_analysis_pb.Target_Key{Label: leaf.Label})
		if !targetValue.IsSet() {
			missingDependencies = true
			continue
		}
		if ruleTarget := targetValue.Message.Definition.GetRuleTarget(); ruleTarget != nil {
			scope = ruleTarget.BuildSetting.GetScope()
		}
		if p.shouldPropagate(buildSettingLabel, scope) {
			propagatingBuildSettings[leaf.Label] = struct{}{}
		}
	}
	if errIter != nil {
		return nil, errIter
	}
	if missingDependencies {
		return nil, evaluation.ErrMissingDependency
	}
	return propagatingBuildSettings, nil
}
