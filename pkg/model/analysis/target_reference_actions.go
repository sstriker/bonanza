package analysis

import (
	"context"
	"errors"
	"fmt"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/btree"
	"bonanza.build/pkg/model/evaluation"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_command_pb "bonanza.build/pkg/proto/model/command"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/storage/object"

	"go.starlark.net/starlark"
)

// targetActionsEnvironment contains the parts of the environments of
// ConfiguredTarget and ConfiguredAspect that are needed to decode the
// actions that a configured target registered into Starlark values.
type targetActionsEnvironment[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata] interface {
	expandFileIfDirectoryEnvironment[TReference, TMetadata]

	GetActionReadersValue(key *model_analysis_pb.ActionReaders_Key) (*ActionReaders[TReference], bool)
	GetConfiguredTargetValue(key model_core.PatchedMessage[*model_analysis_pb.ConfiguredTarget_Key, TMetadata]) model_core.Message[*model_analysis_pb.ConfiguredTarget_Value, TReference]
	GetDirectoryReadersValue(key *model_analysis_pb.DirectoryReaders_Key) (*DirectoryReaders[TReference], bool)
}

// newTargetActionsResolver creates a callback that decodes the actions
// and outputs that are stored in a ConfiguredTarget value into a list
// of Starlark structs resembling Bazel's Action type. The callback is
// only invoked when target.actions is actually accessed, meaning that
// no dependencies on the configured target's value are recorded if the
// caller does not inspect its actions.
func (c *baseComputer[TReference, TMetadata]) newTargetActionsResolver(
	ctx context.Context,
	e targetActionsEnvironment[TReference, TMetadata],
	targetLabel label.CanonicalLabel,
	configurationReference model_core.Message[*model_core_pb.DecodableReference, TReference],
) model_starlark.TargetActionsResolver {
	return func(thread *starlark.Thread) (starlark.Value, error) {
		return c.computeTargetActions(ctx, e, thread, targetLabel, configurationReference)
	}
}

func (c *baseComputer[TReference, TMetadata]) computeTargetActions(
	ctx context.Context,
	e targetActionsEnvironment[TReference, TMetadata],
	thread *starlark.Thread,
	targetLabel label.CanonicalLabel,
	configurationReference model_core.Message[*model_core_pb.DecodableReference, TReference],
) (starlark.Value, error) {
	configuredTarget := e.GetConfiguredTargetValue(
		model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.ConfiguredTarget_Key {
			return &model_analysis_pb.ConfiguredTarget_Key{
				Label:                  targetLabel.String(),
				ConfigurationReference: model_core.Patch(e, configurationReference).Merge(patcher),
			}
		}),
	)
	actionReaders, gotActionReaders := e.GetActionReadersValue(&model_analysis_pb.ActionReaders_Key{})
	directoryReaders, gotDirectoryReaders := e.GetDirectoryReadersValue(&model_analysis_pb.DirectoryReaders_Key{})
	if !configuredTarget.IsSet() || !gotActionReaders || !gotDirectoryReaders {
		return nil, evaluation.ErrMissingDependency
	}

	identifierGenerator, ok := thread.Local(model_starlark.ReferenceEqualIdentifierGeneratorKey).(model_starlark.ReferenceEqualIdentifierGenerator)
	if !ok {
		return nil, errors.New("actions cannot be decoded from within this context")
	}

	// Group the outputs of the configured target by the action that
	// generates them. Outputs created through ctx.actions.write(),
	// expand_template() and symlink() are not backed by actions.
	// Synthesize action objects for those, so that aspects can
	// observe them like Bazel's FileWriteAction, TemplateExpandAction
	// and SymlinkAction.
	targetPackage := targetLabel.GetCanonicalPackage()
	targetNameStr := targetLabel.GetTargetName().String()
	filesByActionID := map[string][]starlark.Value{}
	var synthesizedActions []starlark.Value
	var errIterOutputs error
	for output := range btree.AllLeaves(
		ctx,
		c.configuredTargetOutputReader,
		model_core.Nested(configuredTarget, configuredTarget.Message.Outputs),
		/* traverser = */ func(element model_core.Message[*model_analysis_pb.ConfiguredTarget_Value_Output, TReference]) (*model_core_pb.DecodableReference, error) {
			return element.Message.GetParent().GetReference(), nil
		},
		&errIterOutputs,
	) {
		outputLevel, ok := output.Message.Level.(*model_analysis_pb.ConfiguredTarget_Value_Output_Leaf_)
		if !ok {
			return nil, errors.New("output is not a leaf")
		}
		leaf := outputLevel.Leaf
		packageRelativePath, err := label.NewTargetName(leaf.PackageRelativePath)
		if err != nil {
			return nil, fmt.Errorf("invalid package relative path %#v: %w", leaf.PackageRelativePath, err)
		}
		f := model_starlark.NewFile[TReference, TMetadata](
			model_core.Nested(configurationReference, &model_starlark_pb.File{
				Owner: &model_starlark_pb.File_Owner{
					ConfigurationReference: configurationReference.Message,
					TargetName:             targetNameStr,
					Type:                   leaf.Definition.GetFileType(),
				},
				Label: targetPackage.AppendTargetName(packageRelativePath).String(),
			}),
		)

		switch source := leaf.Definition.GetSource().(type) {
		case *model_analysis_pb.TargetOutputDefinition_ActionId:
			filesByActionID[string(source.ActionId)] = append(filesByActionID[string(source.ActionId)], f)
		case *model_analysis_pb.TargetOutputDefinition_ExpandTemplate_:
			substitutions := starlark.NewDict(len(source.ExpandTemplate.Substitutions))
			for _, substitution := range source.ExpandTemplate.Substitutions {
				if err := substitutions.SetKey(thread, starlark.String(substitution.Needle), starlark.String(substitution.Replacement)); err != nil {
					return nil, err
				}
			}
			action, err := newSynthesizedTargetAction[TReference, TMetadata](thread, identifierGenerator, "TemplateExpand", f, substitutions)
			if err != nil {
				return nil, err
			}
			synthesizedActions = append(synthesizedActions, action)
		case *model_analysis_pb.TargetOutputDefinition_StaticPackageDirectory:
			// Static package directories are only created
			// by ctx.actions.write(). Configured target
			// values that were computed before the
			// introduction of symlink_target_path may also
			// use them for ctx.actions.symlink(
			// target_path=...), which can be told apart by
			// the type of the output.
			mnemonic := "FileWrite"
			if leaf.Definition.GetFileType() == model_starlark_pb.File_Owner_SYMLINK {
				mnemonic = "Symlink"
			}
			action, err := newSynthesizedTargetAction[TReference, TMetadata](thread, identifierGenerator, mnemonic, f, starlark.None)
			if err != nil {
				return nil, err
			}
			synthesizedActions = append(synthesizedActions, action)
		case *model_analysis_pb.TargetOutputDefinition_Write_:
			action, err := newSynthesizedTargetAction[TReference, TMetadata](thread, identifierGenerator, "FileWrite", f, starlark.None)
			if err != nil {
				return nil, err
			}
			synthesizedActions = append(synthesizedActions, action)
		case *model_analysis_pb.TargetOutputDefinition_Symlink_, *model_analysis_pb.TargetOutputDefinition_SymlinkTargetPath_:
			action, err := newSynthesizedTargetAction[TReference, TMetadata](thread, identifierGenerator, "Symlink", f, starlark.None)
			if err != nil {
				return nil, err
			}
			synthesizedActions = append(synthesizedActions, action)
		default:
			return nil, fmt.Errorf("output %#v does not have a known source", leaf.PackageRelativePath)
		}
	}
	if errIterOutputs != nil {
		return nil, errIterOutputs
	}

	// Convert the actions that were registered through
	// ctx.actions.run() and run_shell(). Note that these are stored
	// sorted by action identifier, which may differ from the order
	// in which they were registered by the implementation function.
	var actions []starlark.Value
	var errIterActions error
	for action := range btree.AllLeaves(
		ctx,
		c.configuredTargetActionReader,
		model_core.Nested(configuredTarget, configuredTarget.Message.Actions),
		/* traverser = */ func(element model_core.Message[*model_analysis_pb.ConfiguredTarget_Value_Action, TReference]) (*model_core_pb.DecodableReference, error) {
			return element.Message.GetParent().GetReference(), nil
		},
		&errIterActions,
	) {
		actionLevel, ok := action.Message.Level.(*model_analysis_pb.ConfiguredTarget_Value_Action_Leaf_)
		if !ok {
			return nil, errors.New("action is not a leaf")
		}
		definition := actionLevel.Leaf.Definition
		if definition == nil {
			return nil, errors.New("action definition missing")
		}

		// Expand the arguments of the action to a flat list of
		// strings. The first argument corresponds to the path of
		// the executable.
		var argv []starlark.Value
		if err := c.expandActionArguments(
			ctx,
			e,
			thread,
			directoryReaders,
			model_core.Nested(action, definition.Arguments),
			func(s string) error {
				argv = append(argv, starlark.String(s))
				return nil
			},
		); err != nil {
			return nil, err
		}

		env := starlark.NewDict(len(definition.Env))
		var errIterEnv error
		for environmentVariable := range btree.AllLeaves(
			ctx,
			actionReaders.CommandEnvironmentVariables,
			model_core.Nested(action, definition.Env),
			/* traverser = */ func(element model_core.Message[*model_command_pb.EnvironmentVariableList_Element, TReference]) (*model_core_pb.DecodableReference, error) {
				return element.Message.GetParent(), nil
			},
			&errIterEnv,
		) {
			environmentVariableLevel, ok := environmentVariable.Message.Level.(*model_command_pb.EnvironmentVariableList_Element_Leaf_)
			if !ok {
				return nil, errors.New("environment variable is not a leaf")
			}
			if err := env.SetKey(thread, starlark.String(environmentVariableLevel.Leaf.Name), starlark.String(environmentVariableLevel.Leaf.Value)); err != nil {
				return nil, err
			}
		}
		if errIterEnv != nil {
			return nil, errIterEnv
		}

		inputElements := make([]any, 0, len(definition.Inputs))
		for _, element := range definition.Inputs {
			inputElements = append(inputElements, model_core.Nested(action, element))
		}

		outputs, err := model_starlark.NewDepsetContents[TReference, TMetadata](thread, filesByActionID[string(actionLevel.Leaf.Id)], nil, model_starlark_pb.Depset_DEFAULT)
		if err != nil {
			return nil, err
		}

		mnemonic := definition.Mnemonic
		if mnemonic == "" {
			// Bazel's default mnemonic for actions that were
			// registered through ctx.actions.run() and
			// run_shell().
			mnemonic = "Action"
		}
		actions = append(actions, model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
			"argv":          starlark.NewList(argv),
			"content":       starlark.None,
			"env":           env,
			"inputs":        model_starlark.NewDepset(model_starlark.NewDepsetContentsFromList[TReference, TMetadata](inputElements, model_starlark_pb.Depset_DEFAULT), identifierGenerator),
			"mnemonic":      starlark.String(mnemonic),
			"outputs":       model_starlark.NewDepset(outputs, identifierGenerator),
			"substitutions": starlark.None,
		}))
	}
	if errIterActions != nil {
		return nil, errIterActions
	}

	return starlark.NewList(append(actions, synthesizedActions...)), nil
}

// newSynthesizedTargetAction creates a Starlark struct resembling
// Bazel's Action type for outputs that are not backed by actions, such
// as the ones created through ctx.actions.write(), expand_template()
// and symlink().
func newSynthesizedTargetAction[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata](
	thread *starlark.Thread,
	identifierGenerator model_starlark.ReferenceEqualIdentifierGenerator,
	mnemonic string,
	output starlark.Value,
	substitutions starlark.Value,
) (starlark.Value, error) {
	inputs, err := model_starlark.NewDepsetContents[TReference, TMetadata](thread, nil, nil, model_starlark_pb.Depset_DEFAULT)
	if err != nil {
		return nil, err
	}
	outputs, err := model_starlark.NewDepsetContents[TReference, TMetadata](thread, []starlark.Value{output}, nil, model_starlark_pb.Depset_DEFAULT)
	if err != nil {
		return nil, err
	}
	return model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
		"argv":          starlark.None,
		"content":       starlark.None,
		"env":           starlark.NewDict(0),
		"inputs":        model_starlark.NewDepset(inputs, identifierGenerator),
		"mnemonic":      starlark.String(mnemonic),
		"outputs":       model_starlark.NewDepset(outputs, identifierGenerator),
		"substitutions": substitutions,
	}), nil
}
