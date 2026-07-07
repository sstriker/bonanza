package analysis

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/starlark/unpack"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/protobuf/proto"

	"go.starlark.net/starlark"
)

type getTargetProvidersWithAspectsEnvironment[TReference any, TMetadata model_core.ReferenceMetadata] interface {
	model_core.ObjectManager[TReference, TMetadata]
	GetConfiguredAspectValue(model_core.PatchedMessage[*model_analysis_pb.ConfiguredAspect_Key, TMetadata]) model_core.Message[*model_analysis_pb.ConfiguredAspect_Value, TReference]
	GetTargetProvidersValue(model_core.PatchedMessage[*model_analysis_pb.TargetProviders_Key, TMetadata]) model_core.Message[*model_analysis_pb.TargetProviders_Value, TReference]
}

// getTargetProvidersWithAspects obtains the provider instances of a
// configured target, merged with the provider instances of any aspects
// that the dependency edge along which the target is accessed requests,
// or that an aspect being applied to the target requires. The resulting
// list is sorted alphabetically by provider identifier, which is the
// order in which consumers expect provider instances to be stored.
func getTargetProvidersWithAspects[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata](
	e getTargetProvidersWithAspectsEnvironment[TReference, TMetadata],
	targetLabel string,
	configurationReference model_core.Message[*model_core_pb.DecodableReference, TReference],
	aspectIdentifiers []string,
) (model_core.Message[[]*model_starlark_pb.Struct, TReference], error) {
	// Issue all requests before inspecting any of the results, so
	// that they can be computed in parallel.
	targetProviders := e.GetTargetProvidersValue(
		model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.TargetProviders_Key {
			return &model_analysis_pb.TargetProviders_Key{
				Label:                  targetLabel,
				ConfigurationReference: model_core.Patch(e, configurationReference).Merge(patcher),
			}
		}),
	)

	sortedAspectIdentifiers := slices.Compact(slices.Sorted(slices.Values(aspectIdentifiers)))
	configuredAspects := make([]model_core.Message[*model_analysis_pb.ConfiguredAspect_Value, TReference], 0, len(sortedAspectIdentifiers))
	missingDependencies := false
	for _, aspectIdentifier := range sortedAspectIdentifiers {
		configuredAspect := e.GetConfiguredAspectValue(
			model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.ConfiguredAspect_Key {
				return &model_analysis_pb.ConfiguredAspect_Key{
					AspectIdentifier:       aspectIdentifier,
					Label:                  targetLabel,
					ConfigurationReference: model_core.Patch(e, configurationReference).Merge(patcher),
				}
			}),
		)
		if configuredAspect.IsSet() {
			configuredAspects = append(configuredAspects, configuredAspect)
		} else {
			missingDependencies = true
		}
	}
	if missingDependencies || !targetProviders.IsSet() {
		return model_core.Message[[]*model_starlark_pb.Struct, TReference]{}, evaluation.ErrMissingDependency
	}

	targetProviderInstances := model_core.Nested(targetProviders, targetProviders.Message.ProviderInstances)
	if len(configuredAspects) == 0 {
		return targetProviderInstances, nil
	}

	merged, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) ([]*model_starlark_pb.Struct, error) {
		patchedTargetProviderInstances := model_core.PatchList(e, targetProviderInstances)
		patcher.Merge(patchedTargetProviderInstances.Patcher)
		instances := patchedTargetProviderInstances.Message

		providedByTarget := make(map[string]struct{}, len(instances))
		for _, instance := range instances {
			providedByTarget[instance.ProviderInstanceProperties.GetProviderIdentifier()] = struct{}{}
		}
		providedByAspect := map[string]string{}
		for i, configuredAspect := range configuredAspects {
			aspectIdentifier := sortedAspectIdentifiers[i]
			patchedAspectProviderInstances := model_core.PatchList(e, model_core.Nested(configuredAspect, configuredAspect.Message.ProviderInstances))
			patcher.Merge(patchedAspectProviderInstances.Patcher)
			for _, instance := range patchedAspectProviderInstances.Message {
				providerIdentifier := instance.ProviderInstanceProperties.GetProviderIdentifier()
				if _, ok := providedByTarget[providerIdentifier]; ok {
					return nil, fmt.Errorf("aspect %#v returned provider %#v, which target with label %#v already provides", aspectIdentifier, providerIdentifier, targetLabel)
				}
				if otherAspectIdentifier, ok := providedByAspect[providerIdentifier]; ok {
					return nil, fmt.Errorf("aspects %#v and %#v both returned provider %#v for target with label %#v", otherAspectIdentifier, aspectIdentifier, providerIdentifier, targetLabel)
				}
				providedByAspect[providerIdentifier] = aspectIdentifier
				instances = append(instances, instance)
			}
		}

		slices.SortFunc(instances, func(a, b *model_starlark_pb.Struct) int {
			return strings.Compare(
				a.ProviderInstanceProperties.GetProviderIdentifier(),
				b.ProviderInstanceProperties.GetProviderIdentifier(),
			)
		})
		return instances, nil
	})
	if err != nil {
		return model_core.Message[[]*model_starlark_pb.Struct, TReference]{}, err
	}
	return model_core.Unpatch(e, merged).Decay(), nil
}

type getAspectDefinitionEnvironment[TReference any] interface {
	GetCompiledBzlFileGlobalValue(*model_analysis_pb.CompiledBzlFileGlobal_Key) model_core.Message[*model_analysis_pb.CompiledBzlFileGlobal_Value, TReference]
}

// getAspectDefinition obtains the definition of an aspect, given its
// canonical Starlark identifier.
func getAspectDefinition[TReference any](e getAspectDefinitionEnvironment[TReference], aspectIdentifier string) (model_core.Message[*model_starlark_pb.Aspect_Definition, TReference], error) {
	aspectValue := e.GetCompiledBzlFileGlobalValue(&model_analysis_pb.CompiledBzlFileGlobal_Key{
		Identifier: aspectIdentifier,
	})
	if !aspectValue.IsSet() {
		return model_core.Message[*model_starlark_pb.Aspect_Definition, TReference]{}, evaluation.ErrMissingDependency
	}
	av, ok := aspectValue.Message.Global.GetKind().(*model_starlark_pb.Value_Aspect)
	if !ok {
		return model_core.Message[*model_starlark_pb.Aspect_Definition, TReference]{}, fmt.Errorf("%#v is not an aspect", aspectIdentifier)
	}
	ad, ok := av.Aspect.Kind.(*model_starlark_pb.Aspect_Definition_)
	if !ok {
		return model_core.Message[*model_starlark_pb.Aspect_Definition, TReference]{}, fmt.Errorf("%#v is not an aspect definition", aspectIdentifier)
	}
	return model_core.Nested(aspectValue, ad.Definition), nil
}

// requiredProviderSetsSatisfied returns whether a sorted list of
// advertised providers satisfies at least one of the given sets of
// required providers, meaning that all providers contained in that set
// are advertised. An empty list of sets is trivially satisfied.
func requiredProviderSetsSatisfied(requiredProviderSets []*model_starlark_pb.Aspect_Definition_RequiredProviderSet, advertisedProviders []string) bool {
	if len(requiredProviderSets) == 0 {
		return true
	}
	for _, requiredProviderSet := range requiredProviderSets {
		satisfied := true
		for _, providerIdentifier := range requiredProviderSet.ProviderIdentifiers {
			if _, ok := sort.Find(
				len(advertisedProviders),
				func(i int) int { return strings.Compare(providerIdentifier, advertisedProviders[i]) },
			); !ok {
				satisfied = false
				break
			}
		}
		if satisfied {
			return true
		}
	}
	return false
}

func (c *baseComputer[TReference, TMetadata]) ComputeConfiguredAspectValue(ctx context.Context, key model_core.Message[*model_analysis_pb.ConfiguredAspect_Key, TReference], e ConfiguredAspectEnvironment[TReference, TMetadata]) (PatchedConfiguredAspectValue[TMetadata], error) {
	targetLabel, err := label.NewCanonicalLabel(key.Message.Label)
	if err != nil {
		return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("invalid target label: %w", err)
	}
	aspectIdentifier, err := label.NewCanonicalStarlarkIdentifier(key.Message.AspectIdentifier)
	if err != nil {
		return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("invalid aspect identifier: %w", err)
	}
	configurationReference := model_core.Nested(key, key.Message.ConfigurationReference)

	targetValue := e.GetTargetValue(&model_analysis_pb.Target_Key{
		Label: targetLabel.String(),
	})
	if !targetValue.IsSet() {
		return PatchedConfiguredAspectValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	targetKind, ok := targetValue.Message.Definition.GetKind().(*model_starlark_pb.Target_Definition_RuleTarget)
	if !ok {
		// Aspects are only applied to rule targets. Source
		// files and package groups yield no aspect providers,
		// and aliases have already been expanded by
		// VisibleTarget.
		return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.ConfiguredAspect_Value{}), nil
	}
	ruleTarget := targetKind.RuleTarget

	allBuiltinsModulesNames := e.GetBuiltinsModuleNamesValue(&model_analysis_pb.BuiltinsModuleNames_Key{})
	aspectDefinition, aspectDefinitionErr := getAspectDefinition(e, aspectIdentifier.String())
	if aspectDefinitionErr != nil && !errors.Is(aspectDefinitionErr, evaluation.ErrMissingDependency) {
		return PatchedConfiguredAspectValue[TMetadata]{}, aspectDefinitionErr
	}
	patchedConfigurationReference := model_core.Patch(e, configurationReference)
	targetProviders := e.GetTargetProvidersValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.TargetProviders_Key{
				Label:                  targetLabel.String(),
				ConfigurationReference: patchedConfigurationReference.Message,
			},
			patchedConfigurationReference.Patcher,
		),
	)
	gotCommonDependencies := allBuiltinsModulesNames.IsSet() &&
		aspectDefinitionErr == nil &&
		targetProviders.IsSet()

	var ruleIdentifier label.CanonicalStarlarkIdentifier
	var ruleDefinition model_core.Message[*model_starlark_pb.Rule_Definition, TReference]
	if ruleTarget.RuleIdentifier != "" {
		var err error
		ruleIdentifier, err = label.NewCanonicalStarlarkIdentifier(ruleTarget.RuleIdentifier)
		if err != nil {
			return PatchedConfiguredAspectValue[TMetadata]{}, err
		}

		ruleValue := e.GetCompiledBzlFileGlobalValue(&model_analysis_pb.CompiledBzlFileGlobal_Key{
			Identifier: ruleIdentifier.String(),
		})
		if !gotCommonDependencies || !ruleValue.IsSet() {
			return PatchedConfiguredAspectValue[TMetadata]{}, evaluation.ErrMissingDependency
		}
		rv, ok := ruleValue.Message.Global.GetKind().(*model_starlark_pb.Value_Rule)
		if !ok {
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("%#v is not a rule", ruleIdentifier.String())
		}
		rd, ok := rv.Rule.Kind.(*model_starlark_pb.Rule_Definition_)
		if !ok {
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("%#v is not a rule definition", ruleIdentifier.String())
		}
		ruleDefinition = model_core.Nested(ruleValue, rd.Definition)
	} else if ruleTarget.RuleDefinition != nil {
		// Anonymous rule of which the definition is embedded
		// into the target, as created by
		// testing.analysis_test(). Synthesize an identifier
		// based on the file declaring the implementation
		// function, as it is only used for error messages and
		// to determine the package relative to which default
		// attr values are resolved.
		if !gotCommonDependencies {
			return PatchedConfiguredAspectValue[TMetadata]{}, evaluation.ErrMissingDependency
		}
		implementationFilename, err := label.NewCanonicalLabel(ruleTarget.RuleDefinition.GetImplementation().GetFilename())
		if err != nil {
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("invalid rule implementation function filename: %w", err)
		}
		ruleIdentifier = implementationFilename.AppendStarlarkIdentifier(util.Must(label.NewStarlarkIdentifier("analysis_test")))
		ruleDefinition = model_core.Nested(targetValue, ruleTarget.RuleDefinition)
	} else {
		return PatchedConfiguredAspectValue[TMetadata]{}, errors.New("rule target has neither a rule identifier, nor an inline rule definition")
	}

	// The aspect may require rule targets to advertise certain
	// providers via rule(provides = ...). If the requirements are
	// not met, the aspect is not applied to the target. This also
	// prevents the aspect from propagating through the target, as
	// the values of the target's attrs are no longer configured with
	// the aspect attached.
	if !requiredProviderSetsSatisfied(aspectDefinition.Message.RequiredProviders, ruleDefinition.Message.Provides) {
		return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.ConfiguredAspect_Value{}), nil
	}

	// Compute the transitive closure of aspects that this aspect
	// requires. Required aspects are applied to the same targets as
	// this aspect, prior to this aspect being applied.
	requiresClosure := map[string]model_core.Message[*model_starlark_pb.Aspect_Definition, TReference]{}
	missingRequiredAspectDefinitions := false
	for toVisit := aspectDefinition.Message.Requires; len(toVisit) > 0; {
		var nextToVisit []string
		for _, requiredAspectIdentifier := range toVisit {
			if _, ok := requiresClosure[requiredAspectIdentifier]; ok {
				continue
			}
			requiredAspectDefinition, err := getAspectDefinition(e, requiredAspectIdentifier)
			if err != nil {
				if !errors.Is(err, evaluation.ErrMissingDependency) {
					return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("required aspect %#v: %w", requiredAspectIdentifier, err)
				}
				// Continue to fetch the definitions of
				// any other required aspects, so that
				// they can be computed in parallel.
				missingRequiredAspectDefinitions = true
				continue
			}
			requiresClosure[requiredAspectIdentifier] = requiredAspectDefinition
			nextToVisit = append(nextToVisit, requiredAspectDefinition.Message.Requires...)
		}
		toVisit = nextToVisit
	}
	if missingRequiredAspectDefinitions {
		return PatchedConfiguredAspectValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	if _, ok := requiresClosure[aspectIdentifier.String()]; ok {
		return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("aspect %#v requires itself", aspectIdentifier.String())
	}

	// Determine which of the required aspects' providers should be
	// visible to this aspect: those of aspects that are required
	// directly, and those of aspects advertising providers that
	// satisfy required_aspect_providers.
	var visibleRequiredAspectIdentifiers []string
	for requiredAspectIdentifier, requiredAspectDefinition := range requiresClosure {
		if slices.Contains(aspectDefinition.Message.Requires, requiredAspectIdentifier) ||
			(len(aspectDefinition.Message.RequiredAspectProviders) > 0 &&
				requiredProviderSetsSatisfied(aspectDefinition.Message.RequiredAspectProviders, requiredAspectDefinition.Message.Provides)) {
			visibleRequiredAspectIdentifiers = append(visibleRequiredAspectIdentifiers, requiredAspectIdentifier)
		}
	}

	// Obtain the providers of the target itself, merged with those
	// of the required aspects whose providers are visible, so that
	// the implementation function can access them through the target
	// argument. This is also what causes the required aspects to be
	// applied prior to this aspect.
	missingDependencies := false
	targetProviderInstances, err := getTargetProvidersWithAspects(
		e,
		targetLabel.String(),
		configurationReference,
		visibleRequiredAspectIdentifiers,
	)
	if err != nil {
		if !errors.Is(err, evaluation.ErrMissingDependency) {
			return PatchedConfiguredAspectValue[TMetadata]{}, err
		}
		missingDependencies = true
	}

	thread := c.newStarlarkThread(ctx, e, allBuiltinsModulesNames.Message.BuiltinsModuleNames)
	identifierGenerator, err := c.getReferenceEqualIdentifierGenerator(model_core.Nested(key, proto.Message(key.Message)))
	if err != nil {
		return PatchedConfiguredAspectValue[TMetadata]{}, err
	}
	thread.SetLocal(model_starlark.ReferenceEqualIdentifierGeneratorKey, identifierGenerator)

	// Resolve the execution platforms of the rule's exec groups, as
	// attrs with cfg = "exec" need them to perform their
	// transition.
	targetPackage := targetLabel.GetCanonicalPackage()
	execGroupPlatformLabels := map[string]string{}
	for _, namedExecGroup := range ruleDefinition.Message.ExecGroups {
		execGroupDefinition := namedExecGroup.ExecGroup
		if execGroupDefinition == nil {
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("missing definition of exec group %#v", namedExecGroup.Name)
		}
		execCompatibleWith, err := c.constraintValuesToConstraints(
			ctx,
			e,
			targetPackage,
			execGroupDefinition.ExecCompatibleWith,
		)
		if err != nil {
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("invalid constraint values for exec group %#v: %w", namedExecGroup.Name, err)
		}
		patchedConfigurationReference := model_core.Patch(e, configurationReference)
		resolvedToolchains := e.GetResolvedToolchainsValue(
			model_core.NewPatchedMessage(
				&model_analysis_pb.ResolvedToolchains_Key{
					ExecCompatibleWith:     execCompatibleWith,
					ConfigurationReference: patchedConfigurationReference.Message,
					Toolchains:             execGroupDefinition.Toolchains,
				},
				patchedConfigurationReference.Patcher,
			),
		)
		if !resolvedToolchains.IsSet() {
			missingDependencies = true
			continue
		}
		execGroupPlatformLabels[namedExecGroup.Name] = resolvedToolchains.Message.PlatformLabel
	}

	// Obtain the values of the attrs of the rule target to which
	// the aspect is applied, so that they can be exposed through
	// ctx.rule.attr. For attrs listed in the aspect's attr_aspects,
	// the aspect is also applied to the dependencies, which is what
	// causes it to propagate through the build graph.
	attrValues := make(map[string]any, len(ruleDefinition.Message.Attrs)+3)
	attrValues["name"] = starlark.String(targetLabel.GetTargetName().String())
	tags := make([]starlark.Value, 0, len(ruleTarget.Tags))
	for _, tag := range ruleTarget.Tags {
		tags = append(tags, starlark.String(tag))
	}
	attrValues["tags"] = starlark.NewList(tags)
	attrValues["testonly"] = starlark.Bool(ruleTarget.InheritableAttrs.GetTestonly())

	visibilityValue, err := newVisibilityAttrValue[TReference, TMetadata](targetLabel, ruleTarget.InheritableAttrs)
	if err != nil {
		return PatchedConfiguredAspectValue[TMetadata]{}, err
	}
	attrValues["visibility"] = visibilityValue

	if v, err := c.decodeSelectGroupsAttrValue(ctx, e, thread, model_core.Nested(targetValue, ruleTarget.Features), configurationReference, targetPackage); err != nil {
		if !errors.Is(err, evaluation.ErrMissingDependency) {
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("features: %w", err)
		}
		missingDependencies = true
	} else {
		attrValues["features"] = v
	}
	if v, err := c.decodeSelectGroupsAttrValue(ctx, e, thread, model_core.Nested(targetValue, ruleTarget.TargetCompatibleWith), configurationReference, targetPackage); err != nil {
		if !errors.Is(err, evaluation.ErrMissingDependency) {
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("target_compatible_with: %w", err)
		}
		missingDependencies = true
	} else {
		attrValues["target_compatible_with"] = v
	}

	// In addition to this aspect itself, also apply the required
	// aspects whose providers are visible to dependencies, so that
	// their providers can be accessed through ctx.rule.attr.
	attrAspects := aspectDefinition.Message.AttrAspects
	propagateToAllAttrs := slices.Contains(attrAspects, "*")
	extraAspectIdentifiers := append(slices.Clone(visibleRequiredAspectIdentifiers), aspectIdentifier.String())

	// Note that any incoming edge transition of the rule is not
	// reapplied here, and that select() expressions are resolved
	// against the configuration in which the target is accessed.
	ruleTargetPublicAttrValues := ruleTarget.PublicAttrValues
	for _, namedAttr := range ruleDefinition.Message.Attrs {
		var publicAttrValue *model_starlark_pb.RuleTarget_PublicAttrValue
		if !strings.HasPrefix(namedAttr.Name, "_") {
			if len(ruleTargetPublicAttrValues) == 0 {
				return PatchedConfiguredAspectValue[TMetadata]{}, errors.New("rule target has fewer public attr values than the rule definition has public attrs")
			}
			publicAttrValue = ruleTargetPublicAttrValues[0]
			ruleTargetPublicAttrValues = ruleTargetPublicAttrValues[1:]
		}

		valueParts, usedDefaultValue, err := getAttrValueParts(
			e,
			configurationReference,
			targetPackage,
			model_core.Nested(ruleDefinition, namedAttr),
			model_core.Nested(targetValue, publicAttrValue),
		)
		if err != nil {
			if errors.Is(err, evaluation.ErrMissingDependency) {
				missingDependencies = true
				continue
			}
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("attr %#v: %w", namedAttr.Name, err)
		}

		// Whether an explicit value or a default attr value is
		// used determines how visibility is computed, just like
		// during target configuration.
		visibilityFromPackage := targetPackage
		if usedDefaultValue {
			visibilityFromPackage = ruleIdentifier.GetCanonicalLabel().GetCanonicalPackage()
		}
		var extraAspects []string
		if propagateToAllAttrs || slices.Contains(attrAspects, namedAttr.Name) {
			extraAspects = extraAspectIdentifiers
		}
		attrValue, err := c.configureAttrValueParts(
			ctx,
			e,
			thread,
			model_core.Nested(ruleDefinition, namedAttr),
			valueParts,
			configurationReference,
			visibilityFromPackage,
			execGroupPlatformLabels,
			extraAspects,
		)
		if err != nil {
			if errors.Is(err, evaluation.ErrMissingDependency) {
				missingDependencies = true
				continue
			}
			return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("attr %#v: %w", namedAttr.Name, err)
		}
		attrValues[namedAttr.Name] = attrValue
	}
	if l := len(ruleTargetPublicAttrValues); l != 0 {
		return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("rule target has %d more public attr values than the rule definition has public attrs", l)
	}
	if missingDependencies {
		return PatchedConfiguredAspectValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	target := model_starlark.NewTargetReference[TReference, TMetadata](
		targetLabel.AsResolved(),
		model_starlark.NewConfiguredTargetReference[TReference, TMetadata](
			targetLabel,
			targetProviderInstances,
			c.newTargetActionsResolver(ctx, e, targetLabel, configurationReference),
		),
	)

	configurationComponent, err := model_starlark.ConfigurationReferenceToComponent(configurationReference)
	if err != nil {
		return PatchedConfiguredAspectValue[TMetadata]{}, err
	}
	ctxVar := starlark.NewDict(0)
	ctxVar.Freeze()
	aspectCtx := model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
		// TODO: Provide the values of the aspect's own attrs.
		"attr": model_starlark.NewStructFromDict[TReference, TMetadata](nil, nil),
		"bin_dir": model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
			"path": starlark.String(model_starlark.ComponentStrBazelOut + "/" + configurationComponent + "/" + model_starlark.ComponentStrBin),
		}),
		"fragments": model_starlark.NewStructFromDict[TReference, TMetadata](nil, nil),
		"label":     model_starlark.NewLabel[TReference, TMetadata](targetLabel.AsResolved()),
		"rule": model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
			"attr": model_starlark.NewStructFromDict[TReference, TMetadata](nil, attrValues),
			"kind": starlark.String(ruleIdentifier.GetStarlarkIdentifier().String()),
		}),
		// TODO: Provide the actual Make variables.
		"var": ctxVar,
	})
	aspectCtx.Freeze()

	// Invoke the aspect's implementation function. Unlike rule
	// implementation functions, it is called directly, as it takes
	// (target, ctx) instead of just ctx.
	returnValue, err := starlark.Call(
		thread,
		model_starlark.NewNamedFunction(
			model_starlark.NewProtoNamedFunctionDefinition[TReference, TMetadata](
				model_core.Nested(aspectDefinition, aspectDefinition.Message.Implementation),
			),
		),
		/* args = */ starlark.Tuple{target, aspectCtx},
		/* kwargs = */ nil,
	)
	if err != nil {
		if !errors.Is(err, evaluation.ErrMissingDependency) {
			var evalErr *starlark.EvalError
			if errors.As(err, &evalErr) {
				return PatchedConfiguredAspectValue[TMetadata]{}, errors.New(evalErr.Backtrace())
			}
		}
		return PatchedConfiguredAspectValue[TMetadata]{}, err
	}

	// Bazel permits returning either a single provider, or a list
	// of providers.
	var providerInstances []*model_starlark.Struct[TReference, TMetadata]
	structUnpackerInto := unpack.Type[*model_starlark.Struct[TReference, TMetadata]]("struct")
	if err := unpack.IfNotNone(
		unpack.Or([]unpack.UnpackerInto[[]*model_starlark.Struct[TReference, TMetadata]]{
			unpack.Singleton(structUnpackerInto),
			unpack.List(structUnpackerInto),
		}),
	).UnpackInto(thread, returnValue, &providerInstances); err != nil {
		return PatchedConfiguredAspectValue[TMetadata]{}, fmt.Errorf("failed to unpack implementation function return value: %w", err)
	}

	return model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) (*model_analysis_pb.ConfiguredAspect_Value, error) {
		providersSeen := make(map[label.CanonicalStarlarkIdentifier]struct{}, len(providerInstances))
		encodedProviderInstances := make([]*model_starlark_pb.Struct, 0, len(providerInstances))
		for i, providerInstance := range providerInstances {
			providerIdentifier, err := providerInstance.GetProviderIdentifier()
			if err != nil {
				return nil, fmt.Errorf("struct returned at index %d: %w", i, err)
			}
			if _, ok := providersSeen[providerIdentifier]; ok {
				return nil, fmt.Errorf("implementation function returned multiple structs for provider %#v", providerIdentifier.String())
			}
			providersSeen[providerIdentifier] = struct{}{}

			// Bazel does not permit aspects to return
			// DefaultInfo, as every configured target
			// already provides one of its own.
			if providerIdentifier == defaultInfoProviderIdentifier {
				return nil, errors.New("implementation function returned DefaultInfo, which aspects may not provide")
			}

			v, _, err := providerInstance.Encode(map[starlark.Value]struct{}{}, c.getValueEncodingOptions(ctx, e, nil))
			if err != nil {
				return nil, err
			}
			encodedProviderInstances = append(encodedProviderInstances, v.Merge(patcher))
		}

		slices.SortFunc(encodedProviderInstances, func(a, b *model_starlark_pb.Struct) int {
			return strings.Compare(
				a.ProviderInstanceProperties.ProviderIdentifier,
				b.ProviderInstanceProperties.ProviderIdentifier,
			)
		})

		return &model_analysis_pb.ConfiguredAspect_Value{
			ProviderInstances: encodedProviderInstances,
		}, nil
	})
}
