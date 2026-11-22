package analysis

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
)

func (baseComputer[TReference, TMetadata]) ComputeUsedModuleExtensionValue(ctx context.Context, key *model_analysis_pb.UsedModuleExtension_Key, e UsedModuleExtensionEnvironment[TReference, TMetadata]) (PatchedUsedModuleExtensionValue[TMetadata], error) {
	usedModuleExtensions := e.GetUsedModuleExtensionsValue(&model_analysis_pb.UsedModuleExtensions_Key{})
	if !usedModuleExtensions.IsSet() {
		return PatchedUsedModuleExtensionValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	extensions := usedModuleExtensions.Message.ModuleExtensions
	getName := func(i int) (string, error) {
		identifier, err := label.NewCanonicalStarlarkIdentifier(extensions[i].Identifier)
		if err != nil {
			return "", fmt.Errorf("invalid module extensions Starlark identifier %#v: %w", extensions[i].Identifier, err)
		}
		return identifier.ToModuleExtension().String(), nil
	}
	first := sort.Search(
		len(extensions),
		func(i int) bool {
			name, err := getName(i)
			return err != nil || name >= key.ModuleExtension
		},
	)
	patcher := model_core.NewReferenceMessagePatcher[TMetadata]()
	var matchingExtensions []*model_analysis_pb.ModuleExtension
	for i := first; i < len(extensions); i++ {
		name, err := getName(i)
		if err != nil {
			return PatchedUsedModuleExtensionValue[TMetadata]{}, err
		}
		if name != key.ModuleExtension {
			break
		}
		patchedExtension := model_core.Patch(e, model_core.Nested(usedModuleExtensions, extensions[i]))
		patcher.Merge(patchedExtension.Patcher)
		matchingExtensions = append(matchingExtensions, patchedExtension.Message)
	}
	if len(matchingExtensions) > 0 {
		return model_core.NewPatchedMessage(
			&model_analysis_pb.UsedModuleExtension_Value{
				ModuleExtensions: matchingExtensions,
			},
			patcher,
		), nil
	}
	return PatchedUsedModuleExtensionValue[TMetadata]{}, errors.New("module extension not found")
}

// UsedModuleExtensionEnvironmentForTesting is used to generate mocks
// for unit testing BaseComputer.
type UsedModuleExtensionEnvironmentForTesting UsedModuleExtensionEnvironment[model_core.CreatedObjectTree, model_core.CreatedObjectTree]
