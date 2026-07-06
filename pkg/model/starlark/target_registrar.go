package starlark

import (
	"context"
	"fmt"
	"maps"
	"slices"

	pg_label "bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/inlinedtree"
	model_encoding "bonanza.build/pkg/model/encoding"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/storage/object"

	"go.starlark.net/starlark"
)

// TargetRegistrar can be called into by functions like alias(),
// exports_files(), label_flag(), label_setting(), package_group() and
// invocations of rules to register any targets in the current package.
type TargetRegistrar[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata] struct {
	// Immutable fields.
	context            context.Context
	encoder            model_encoding.DeterministicBinaryEncoder
	inlinedTreeOptions *inlinedtree.Options
	objectManager      model_core.ObjectManager[TReference, TMetadata]

	// Mutable fields.
	defaultInheritableAttrs    model_core.Message[*model_starlark_pb.InheritableAttrs, TReference]
	setDefaultInheritableAttrs bool
	targets                    map[string]model_core.PatchedMessage[*model_starlark_pb.Target_Definition, TMetadata]
	existingRules              map[string]map[string]starlark.Value
}

// NewTargetRegistrar creates a TargetRegistrar that at the time of
// creation contains no targets. The caller needs to provide default
// values for attributes that are provided to calls to repo() in
// REPO.bazel, so that they can be inherited by registered targets.
func NewTargetRegistrar[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata](ctx context.Context, encoder model_encoding.DeterministicBinaryEncoder, inlinedTreeOptions *inlinedtree.Options, objectManager model_core.ObjectManager[TReference, TMetadata], defaultInheritableAttrs model_core.Message[*model_starlark_pb.InheritableAttrs, TReference]) *TargetRegistrar[TReference, TMetadata] {
	return &TargetRegistrar[TReference, TMetadata]{
		context:                 ctx,
		encoder:                 encoder,
		inlinedTreeOptions:      inlinedTreeOptions,
		objectManager:           objectManager,
		defaultInheritableAttrs: defaultInheritableAttrs,
		targets:                 map[string]model_core.PatchedMessage[*model_starlark_pb.Target_Definition, TMetadata]{},
		existingRules:           map[string]map[string]starlark.Value{},
	}
}

// registerExistingRule records information on a rule target that was
// instantiated in the current package, so that it can be reported
// through native.existing_rule() and native.existing_rules().
func (tr *TargetRegistrar[TReference, TMetadata]) registerExistingRule(name string, attrs map[string]starlark.Value) {
	tr.existingRules[name] = attrs
}

// GetExistingRule returns information on a previously instantiated rule
// target with a given name, or nil if no rule target with that name
// exists in the current package.
func (tr *TargetRegistrar[TReference, TMetadata]) GetExistingRule(name string) map[string]starlark.Value {
	return tr.existingRules[name]
}

// GetExistingRules returns information on all rule targets that have
// been instantiated in the current package so far.
func (tr *TargetRegistrar[TReference, TMetadata]) GetExistingRules() map[string]map[string]starlark.Value {
	return tr.existingRules
}

// Discard all targets, so that any resources associated with them are
// released.
func (tr *TargetRegistrar[TReference, TMetadata]) Discard() {
	for _, target := range tr.targets {
		target.Discard()
	}
}

// GetTargetNames returns the set of target names in the current package
// that have been registered against this TargetRegistrar.
func (tr *TargetRegistrar[TReference, TMetadata]) GetTargetNames() []string {
	return slices.Sorted(maps.Keys(tr.targets))
}

// GetAndRemoveTarget gets the definition of the target from the
// TargetRegistrar and subsequently removes it. The caller then owns the
// message and its associated resources.
func (tr *TargetRegistrar[TReference, TMetadata]) GetAndRemoveTarget(name string) model_core.PatchedMessage[*model_starlark_pb.Target_Definition, TMetadata] {
	target := tr.targets[name]
	delete(tr.targets, name)
	if !target.IsSet() {
		// Target is referenced, but not provided explicitly.
		// Assume it refers to a source file. Unless
		// --incompatible_no_implicit_file_export takes effect,
		// Bazel gives such files the package's default
		// visibility.
		visibilityPackageGroup := model_core.Patch(
			tr.objectManager,
			model_core.Nested(tr.defaultInheritableAttrs, tr.defaultInheritableAttrs.Message.Visibility),
		)
		target = model_core.NewPatchedMessage(
			&model_starlark_pb.Target_Definition{
				Kind: &model_starlark_pb.Target_Definition_SourceFileTarget{
					SourceFileTarget: &model_starlark_pb.SourceFileTarget{
						Visibility: visibilityPackageGroup.Message,
					},
				},
			},
			visibilityPackageGroup.Patcher,
		)
	}
	return target
}

func (tr *TargetRegistrar[TReference, TMetadata]) getVisibilityPackageGroup(visibility []pg_label.ResolvedLabel) (model_core.PatchedMessage[*model_starlark_pb.PackageGroup, TMetadata], error) {
	if len(visibility) > 0 {
		// Explicit visibility provided. Construct new package group.
		return NewPackageGroupFromVisibility[TMetadata](tr.context, visibility, tr.encoder, tr.inlinedTreeOptions, tr.objectManager)
	}

	// Inherit visibility from repo() in the REPO.bazel file
	// or package() in the BUILD.bazel file.
	return model_core.Patch(
		tr.objectManager,
		model_core.Nested(tr.defaultInheritableAttrs, tr.defaultInheritableAttrs.Message.Visibility),
	), nil
}

func (tr *TargetRegistrar[TReference, TMetadata]) registerExplicitTarget(name string, target model_core.PatchedMessage[*model_starlark_pb.Target_Definition, TMetadata]) error {
	if tr.targets[name].IsSet() {
		return fmt.Errorf("package contains multiple targets with name %#v", name)
	}
	tr.targets[name] = target
	return nil
}

func (tr *TargetRegistrar[TReference, TMetadata]) registerImplicitTarget(name string) {
	if _, ok := tr.targets[name]; !ok {
		tr.targets[name] = model_core.PatchedMessage[*model_starlark_pb.Target_Definition, TMetadata]{}
	}
}
