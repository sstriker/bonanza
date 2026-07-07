package analysis

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/btree"
	"bonanza.build/pkg/model/evaluation"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_command_pb "bonanza.build/pkg/proto/model/command"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

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
	GetFileReaderValue(key *model_analysis_pb.FileReader_Key) (*model_filesystem.FileReader[TReference], bool)
}

// maxActionContentSizeBytes is the maximum size of file contents that
// are exposed through the content field of the Action structs that are
// synthesized for outputs created through ctx.actions.write() and
// expand_template(). Bazel does not limit the size of the content, but
// as the contents need to be loaded into memory in their entirety,
// larger files are treated as not being computable during the analysis
// phase, causing content to be None.
const maxActionContentSizeBytes = 1 << 22

// errActionContentTooLarge is a sentinel that terminates argument
// expansion once the rendered parameter file contents exceed
// maxActionContentSizeBytes.
var errActionContentTooLarge = errors.New("action content exceeds maximum size")

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
	fileReader, gotFileReader := e.GetFileReaderValue(&model_analysis_pb.FileReader_Key{})
	if !configuredTarget.IsSet() || !gotActionReaders || !gotDirectoryReaders || !gotFileReader {
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
		fileDefinition := model_core.Nested(configurationReference, &model_starlark_pb.File{
			Owner: &model_starlark_pb.File_Owner{
				ConfigurationReference: configurationReference.Message,
				TargetName:             targetNameStr,
				Type:                   leaf.Definition.GetFileType(),
			},
			Label: targetPackage.AppendTargetName(packageRelativePath).String(),
		})
		f := model_starlark.NewFile[TReference, TMetadata](fileDefinition)

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

			// Instead of expanding the template here, request a
			// file root containing the output file. This reuses
			// the Merkle tree that is needed to build the target,
			// so the content is guaranteed to consist of the same
			// bytes as the file that is built. This may trigger
			// execution of the action producing the template if
			// the template is itself a derived file. This is safe
			// against cycles, as target actions resolvers are only
			// attached to target references of dependencies and
			// aspect targets, never to the configured target that
			// is currently being computed.
			fileProperties, err := getStarlarkFileProperties(ctx, e, fileDefinition)
			if err != nil {
				return nil, fmt.Errorf("failed to obtain file properties of expanded template %#v: %w", leaf.PackageRelativePath, err)
			}
			content, err := readFileContentsAsStarlarkContent(ctx, fileReader, fileProperties)
			if err != nil {
				return nil, fmt.Errorf("failed to read contents of expanded template %#v: %w", leaf.PackageRelativePath, err)
			}
			action, err := newSynthesizedTargetAction[TReference, TMetadata](thread, identifierGenerator, "TemplateExpand", f, content, substitutions)
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
			content := starlark.Value(starlark.None)
			if leaf.Definition.GetFileType() == model_starlark_pb.File_Owner_SYMLINK {
				// Bazel's SymlinkAction does not expose
				// any content.
				mnemonic = "Symlink"
			} else {
				// The contents of the output file were
				// already computed when
				// ctx.actions.write(content=<str>) was
				// called, and are stored in the package
				// directory embedded in the configured
				// target's value.
				content, err = readPackageDirectoryFileContents(ctx, directoryReaders, fileReader, model_core.Nested(output, source.StaticPackageDirectory), leaf.PackageRelativePath)
				if err != nil {
					return nil, fmt.Errorf("failed to read contents of output %#v: %w", leaf.PackageRelativePath, err)
				}
			}
			action, err := newSynthesizedTargetAction[TReference, TMetadata](thread, identifierGenerator, mnemonic, f, content, starlark.None)
			if err != nil {
				return nil, err
			}
			synthesizedActions = append(synthesizedActions, action)
		case *model_analysis_pb.TargetOutputDefinition_Write_:
			content, err := c.getWriteContent(ctx, e, thread, directoryReaders, model_core.Nested(output, source.Write))
			if err != nil {
				return nil, fmt.Errorf("failed to compute content of output %#v: %w", leaf.PackageRelativePath, err)
			}
			action, err := newSynthesizedTargetAction[TReference, TMetadata](thread, identifierGenerator, "FileWrite", f, content, starlark.None)
			if err != nil {
				return nil, err
			}
			synthesizedActions = append(synthesizedActions, action)
		case *model_analysis_pb.TargetOutputDefinition_Symlink_, *model_analysis_pb.TargetOutputDefinition_SymlinkTargetPath_:
			action, err := newSynthesizedTargetAction[TReference, TMetadata](thread, identifierGenerator, "Symlink", f, starlark.None, starlark.None)
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
	content starlark.Value,
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
		"content":       content,
		"env":           starlark.NewDict(0),
		"inputs":        model_starlark.NewDepset(inputs, identifierGenerator),
		"mnemonic":      starlark.String(mnemonic),
		"outputs":       model_starlark.NewDepset(outputs, identifierGenerator),
		"substitutions": substitutions,
	}), nil
}

// readFileContentsAsStarlarkContent reads the full contents of an
// output file and converts them to a Starlark string, so that they can
// be exposed through the content field of a synthesized Action struct.
// None is returned if the file exceeds maxActionContentSizeBytes.
func readFileContentsAsStarlarkContent[TReference object.BasicReference](
	ctx context.Context,
	fileReader *model_filesystem.FileReader[TReference],
	fileProperties model_core.Message[*model_filesystem_pb.FileProperties, TReference],
) (starlark.Value, error) {
	fileContentsEntry, err := model_filesystem.NewFileContentsEntryFromProto(
		model_core.Nested(fileProperties, fileProperties.Message.GetContents()),
	)
	if err != nil {
		return nil, err
	}
	if fileContentsEntry.GetEndBytes() > maxActionContentSizeBytes {
		return starlark.None, nil
	}
	data, err := fileReader.FileReadAll(ctx, fileContentsEntry, maxActionContentSizeBytes)
	if err != nil {
		return nil, err
	}
	return starlark.String(data), nil
}

// readPackageDirectoryFileContents looks up a file contained in a
// static package directory that is stored in a configured target's
// value and reads its contents. None is returned if the path does not
// resolve to a regular file, which is the case for outputs of
// ctx.actions.symlink(target_path=...) that were computed before
// symlink_target_path was introduced.
func readPackageDirectoryFileContents[TReference object.BasicReference](
	ctx context.Context,
	directoryReaders *DirectoryReaders[TReference],
	fileReader *model_filesystem.FileReader[TReference],
	packageDirectory model_core.Message[*model_filesystem_pb.DirectoryContents, TReference],
	packageRelativePath string,
) (starlark.Value, error) {
	componentWalker := model_filesystem.NewDirectoryComponentWalker[TReference](
		ctx,
		directoryReaders.DirectoryContents,
		directoryReaders.Leaves,
		func() (path.ComponentWalker, error) {
			return nil, errors.New("path resolution escapes package directory")
		},
		model_core.Message[*model_filesystem_pb.Directory, TReference]{},
		[]model_core.Message[*model_filesystem_pb.DirectoryContents, TReference]{packageDirectory},
	)
	if err := path.Resolve(
		path.UNIXFormat.NewParser(packageRelativePath),
		path.NewLoopDetectingScopeWalker(path.NewRelativeScopeWalker(componentWalker)),
	); err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}
	fileProperties := componentWalker.GetCurrentFileProperties()
	if !fileProperties.IsSet() {
		return starlark.None, nil
	}
	return readFileContentsAsStarlarkContent(ctx, fileReader, fileProperties)
}

// getWriteContent renders the arguments stored by
// ctx.actions.write(content=<Args>) to the parameter file contents
// that building the output would yield, so that they can be exposed
// through the content field of the synthesized Action struct.
//
// Mirroring Bazel's ParameterFileWriteAction, None is returned if the
// arguments reference directories, as expanding those may require the
// actions producing them to be executed. None is also returned if the
// rendered contents exceed maxActionContentSizeBytes, or if the
// arguments use the FLAG_PER_LINE format, whose rendering is currently
// not implemented.
func (c *baseComputer[TReference, TMetadata]) getWriteContent(
	ctx context.Context,
	e targetActionsEnvironment[TReference, TMetadata],
	thread *starlark.Thread,
	directoryReaders *DirectoryReaders[TReference],
	write model_core.Message[*model_analysis_pb.TargetOutputDefinition_Write, TReference],
) (starlark.Value, error) {
	switch write.Message.GetParamFileFormat() {
	case model_analysis_pb.Args_Leaf_UseParamFile_MULTILINE,
		model_analysis_pb.Args_Leaf_UseParamFile_SHELL:
	default:
		return starlark.None, nil
	}

	referencesDirectory, err := c.argsLeafReferencesDirectory(ctx, thread, model_core.Nested(write, write.Message.Content))
	if err != nil {
		return nil, err
	}
	if referencesDirectory {
		return starlark.None, nil
	}

	var content strings.Builder
	if err := c.expandActionArguments(
		ctx,
		e,
		thread,
		directoryReaders,
		model_core.Nested(write, []*model_analysis_pb.Args{{
			Level: &model_analysis_pb.Args_Leaf_{
				Leaf: write.Message.Content,
			},
		}}),
		func(argument string) error {
			if err := appendParamFileArgument(&content, write.Message.ParamFileFormat, argument); err != nil {
				return err
			}
			if content.Len() > maxActionContentSizeBytes {
				return errActionContentTooLarge
			}
			return nil
		},
	); err != nil {
		if errors.Is(err, errActionContentTooLarge) {
			return starlark.None, nil
		}
		return nil, err
	}
	return starlark.String(content.String()), nil
}

// argsLeafReferencesDirectory scans the values that were added to an
// Args object with expand_directories=True for Files that refer to
// directories. Rendering such arguments requires the directories'
// contents, which may only become available during execution. Bazel's
// ParameterFileWriteAction reports content None in that case.
func (c *baseComputer[TReference, TMetadata]) argsLeafReferencesDirectory(
	ctx context.Context,
	thread *starlark.Thread,
	argsLeaf model_core.Message[*model_analysis_pb.Args_Leaf, TReference],
) (bool, error) {
	valueDecodingOptions := c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
		return model_starlark.NewLabel[TReference, TMetadata](resolvedLabel), nil
	})
	var errIterAdd error
	for add := range btree.AllLeaves(
		ctx,
		c.argsAddReader,
		model_core.Nested(argsLeaf, argsLeaf.Message.GetAdds()),
		/* traverser = */ func(element model_core.Message[*model_analysis_pb.Args_Leaf_Add, TReference]) (*model_core_pb.DecodableReference, error) {
			return element.Message.GetParent().GetReference(), nil
		},
		&errIterAdd,
	) {
		addLeaf, ok := add.Message.Level.(*model_analysis_pb.Args_Leaf_Add_Leaf_)
		if !ok {
			return false, errors.New("args.add*() entry is not a leaf")
		}
		if !addLeaf.Leaf.ExpandDirectories {
			// Directory Files are only replaced by their
			// contents if expand_directories=True is provided
			// to args.add_all() or args.add_joined().
			// Otherwise they are rendered as plain paths,
			// which does not require their contents.
			continue
		}
		values, err := model_starlark.DecodeValue[TReference, TMetadata](
			model_core.Nested(add, addLeaf.Leaf.Values),
			/* currentIdentifier = */ nil,
			valueDecodingOptions,
		)
		if err != nil {
			return false, err
		}
		var elements iter.Seq[starlark.Value]
		switch typedValues := values.(type) {
		case *model_starlark.Depset[TReference, TMetadata]:
			list, err := typedValues.ToList(thread)
			if err != nil {
				return false, err
			}
			elements = slices.Values(list)
		case starlark.Iterable:
			elements = starlark.Elements(typedValues)
		default:
			return false, errors.New("args.add*() value is not a depset or list")
		}
		for v := range elements {
			if f, ok := v.(*model_starlark.File[TReference, TMetadata]); ok {
				if o := f.GetDefinition().Message.Owner; o != nil && o.Type == model_starlark_pb.File_Owner_DIRECTORY {
					return true, nil
				}
			}
		}
	}
	if errIterAdd != nil {
		return false, errIterAdd
	}
	return false, nil
}
